package authz

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
)

type Scope struct {
	Tx     pgx.Tx
	UserID string
	JobID  string
	OrgID  string
	Role   v1.Role
	IP     string
	// BuildServerID is set for a builder's stream, and CertExpires to when
	// its certificate ends.
	BuildServerID string
	CertExpires   time.Time
	// AgentSessionID is set for a toolbox's stream.
	AgentSessionID string
	target         string
	after          []func()
}

type scopeKey struct{}

func ScopeFrom(ctx context.Context) *Scope {
	return ctx.Value(scopeKey{}).(*Scope)
}

func (s *Scope) SetUser(ctx context.Context, userID string) error {
	if _, err := s.Tx.Exec(ctx, `SELECT set_config('stackwright.user_id', $1, true)`, userID); err != nil {
		return err
	}
	s.UserID = userID
	return nil
}

func (s *Scope) SetOrg(ctx context.Context, orgID string) error {
	if _, err := s.Tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
		return err
	}
	s.OrgID = orgID
	return nil
}

// Target names what a write changed, for its audit row. Every write sets it.
func (s *Scope) Target(t string) {
	s.target = t
}

// AfterCommit runs f once the request's transaction has committed.
func (s *Scope) AfterCommit(f func()) {
	s.after = append(s.after, f)
}

// AfterCommit runs f after the request's transaction commits, or now when
// ctx carries no request, such as in a background loop.
func AfterCommit(ctx context.Context, f func()) {
	if s, ok := ctx.Value(scopeKey{}).(*Scope); ok && s.Tx != nil {
		s.AfterCommit(f)
		return
	}
	f()
}

// Read runs fn in a new transaction with the scope's user and org. A
// streaming handler has no request transaction, and uses this instead.
func (s *Scope) Read(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.user_id', $1, true), set_config('stackwright.org_id', $2, true)`,
			s.UserID, s.OrgID); err != nil {
			return err
		}
		return fn(tx)
	})
}

type Interceptor struct {
	Pool *pgxpool.Pool
}

type peerCertKey struct{}

// WithPeerCertificate records a client certificate that the TLS handshake
// verified against the builder CA.
func WithPeerCertificate(ctx context.Context, cert *x509.Certificate) context.Context {
	return context.WithValue(ctx, peerCertKey{}, cert)
}

// checkOrg sets the scope's org from slug, if the user's role there is min
// or above. s.Tx needs the user setting.
func checkOrg(ctx context.Context, s *Scope, slug string, min v1.Role) error {
	var orgID, role string
	err := s.Tx.QueryRow(ctx, `SELECT o.id, m.role FROM orgs o JOIN memberships m ON m.org_id = o.id
		WHERE o.slug = $1 AND m.user_id = app_user_id()`, slug).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return connect.NewError(connect.CodeNotFound, errors.New("org not found"))
	}
	if err != nil {
		return Internal(err)
	}
	s.Role = RoleFromDB(role)
	if err := checkSSO(ctx, s.Tx, orgID, slug, s.Role); err != nil {
		return err
	}
	if !AtLeast(s.Role, min) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("this needs the %s role or above", RoleToDB(min)))
	}
	if err := s.SetOrg(ctx, orgID); err != nil {
		return Internal(err)
	}
	return nil
}

// ErrNeedsSSO is a member of an org that requires its identity provider,
// signed in another way.
var ErrNeedsSSO = errors.New("signs in through its identity provider; sign in with SSO")

// checkSSO refuses a session or CLI token that an org's SAML sign-in made
// for another org, and, in an org that requires SSO, one that its
// identity provider did not sign in, unless the member is an owner
// (adr/0045).
func checkSSO(ctx context.Context, tx pgx.Tx, orgID, slug string, role v1.Role) error {
	u, _ := auth.UserFrom(ctx)
	if u.SSOOrg != "" && u.SSOOrg != orgID {
		return connect.NewError(connect.CodePermissionDenied, errors.New("this sign-in came through another org's identity provider, and it opens only that org"))
	}
	var required bool
	err := tx.QueryRow(ctx, `SELECT required FROM org_saml WHERE org_id = $1`, orgID).Scan(&required)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Internal(err)
	}
	if required && u.SSOOrg != orgID && role != v1.Role_ROLE_OWNER {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("org %s %w", slug, ErrNeedsSSO))
	}
	return nil
}

func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		procedure := req.Spec().Procedure
		rule, ok := Policy[procedure]
		if !ok {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s has no policy", procedure))
		}
		user, signedIn := auth.UserFrom(ctx)
		jobAccess := rule.Access == JobTicket || rule.Access == Job
		if rule.Access != Public && !jobAccess && !signedIn {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not signed in"))
		}

		tx, err := i.Pool.Begin(ctx)
		if err != nil {
			return nil, Internal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		s := &Scope{Tx: tx, IP: user.IP}
		if signedIn {
			if err := s.SetUser(ctx, user.ID); err != nil {
				return nil, Internal(err)
			}
		}

		switch rule.Access {
		case Public, SignedIn:
		case InstanceAdmin:
			var admin bool
			if err := tx.QueryRow(ctx, `SELECT instance_admin FROM users WHERE id = $1`, user.ID).Scan(&admin); err != nil {
				return nil, Internal(err)
			}
			if !admin || user.SSOOrg != "" {
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only an instance admin may do this, signed in to the instance"))
			}
		case JobTicket, Job:
			bearer, ok := strings.CutPrefix(req.Header().Get("Authorization"), "Bearer ")
			if !ok || bearer == "" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no job credential"))
			}
			query := `UPDATE job_leases SET heartbeat_at = now() WHERE token_hash = $1 AND expires_at > now() RETURNING job_id, org_id`
			if rule.Access == JobTicket {
				query = `UPDATE job_leases SET ticket_hash = NULL WHERE ticket_hash = $1 AND expires_at > now() RETURNING job_id, org_id`
			}
			var orgID string
			err := tx.QueryRow(ctx, query, auth.HashToken(bearer)).Scan(&s.JobID, &orgID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the job credential is unknown, spent or expired"))
			}
			if err != nil {
				return nil, Internal(err)
			}
			if err := s.SetOrg(ctx, orgID); err != nil {
				return nil, Internal(err)
			}
		case InOrg:
			msg, ok := req.Any().(interface{ GetOrg() string })
			if !ok {
				return nil, Internal(fmt.Errorf("%s is org-scoped but its request has no org", procedure))
			}
			if err := checkOrg(ctx, s, msg.GetOrg(), rule.Min); err != nil {
				return nil, err
			}
		default:
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s cannot be called this way", procedure))
		}

		resp, err := next(context.WithValue(ctx, scopeKey{}, s), req)
		if err != nil {
			return nil, err
		}
		if req.Spec().IdempotencyLevel != connect.IdempotencyNoSideEffects && !jobAccess {
			if s.target == "" {
				return nil, Internal(fmt.Errorf("%s wrote without an audit target", procedure))
			}
			actorKind, actor := "user", s.UserID
			if actor == "" {
				actorKind, actor = "system", "anonymous"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
				VALUES (nullif($1, '')::uuid, $2, $3, $4, $5, nullif($6, '')::inet)`,
				s.OrgID, actorKind, actor, strings.TrimPrefix(procedure, "/stackwright.v1."), s.target, s.IP); err != nil {
				return nil, Internal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, Internal(err)
		}
		for _, f := range s.after {
			f()
		}
		return resp, nil
	}
}

func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		procedure := conn.Spec().Procedure
		rule, ok := Policy[procedure]
		switch {
		case !ok:
			return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s has no policy", procedure))
		case rule.Access == Builder:
			cert, _ := ctx.Value(peerCertKey{}).(*x509.Certificate)
			if cert == nil {
				return connect.NewError(connect.CodeUnauthenticated, errors.New("a build server certificate is required"))
			}
			s := &Scope{CertExpires: cert.NotAfter}
			var orgID *string
			err := i.Pool.QueryRow(ctx, `SELECT build_server_id, org_id FROM build_server_certs
				WHERE serial = $1 AND build_server_id::text = $2 AND revoked_at IS NULL AND expires_at > now()`,
				cert.SerialNumber.Text(16), cert.Subject.CommonName).Scan(&s.BuildServerID, &orgID)
			if errors.Is(err, pgx.ErrNoRows) {
				return connect.NewError(connect.CodeUnauthenticated, errors.New("the build server certificate is revoked or expired"))
			}
			if err != nil {
				return Internal(err)
			}
			if orgID != nil {
				s.OrgID = *orgID
			}
			return next(context.WithValue(ctx, scopeKey{}, s), conn)
		case rule.Access == Toolbox:
			bearer, _ := strings.CutPrefix(conn.RequestHeader().Get("Authorization"), "Bearer ")
			session, orgID, err := SandboxToken(ctx, i.Pool, bearer)
			if err != nil {
				return err
			}
			return next(context.WithValue(ctx, scopeKey{}, &Scope{AgentSessionID: session, OrgID: orgID}), conn)
		case rule.Access == InOrg && conn.Spec().StreamType == connect.StreamTypeServer:
			user, signedIn := auth.UserFrom(ctx)
			if !signedIn {
				return connect.NewError(connect.CodeUnauthenticated, errors.New("not signed in"))
			}
			s := &Scope{UserID: user.ID, IP: user.IP}
			checked := &orgCheckedConn{StreamingHandlerConn: conn, check: func(msg any) error {
				m, ok := msg.(interface{ GetOrg() string })
				if !ok {
					return Internal(fmt.Errorf("%s is org-scoped but its request has no org", procedure))
				}
				return pgx.BeginFunc(ctx, i.Pool, func(tx pgx.Tx) error {
					s.Tx = tx
					defer func() { s.Tx = nil }()
					if err := s.SetUser(ctx, user.ID); err != nil {
						return Internal(err)
					}
					return checkOrg(ctx, s, m.GetOrg(), rule.Min)
				})
			}}
			return next(context.WithValue(ctx, scopeKey{}, s), checked)
		}
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s cannot be called this way", procedure))
	}
}

// orgCheckedConn checks the org in a server stream's one request before the
// handler sees it. Connect reads that request before it calls the handler.
type orgCheckedConn struct {
	connect.StreamingHandlerConn
	check   func(any) error
	checked bool
}

func (c *orgCheckedConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	if c.checked {
		return nil
	}
	c.checked = true
	return c.check(msg)
}

func Internal(err error) error {
	slog.Error("rpc failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func RoleToDB(r v1.Role) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "ROLE_"))
}

func RoleFromDB(s string) v1.Role {
	return v1.Role(v1.Role_value["ROLE_"+strings.ToUpper(s)])
}

// SandboxToken returns the agent session and org of a running sandbox's
// token. The toolbox stream and the Git proxy both take it.
func SandboxToken(ctx context.Context, pool *pgxpool.Pool, token string) (session, orgID string, err error) {
	if token == "" {
		return "", "", connect.NewError(connect.CodeUnauthenticated, errors.New("no sandbox token"))
	}
	err = pool.QueryRow(ctx, `SELECT session_id, org_id FROM agent_sandbox_tokens WHERE token_hash = $1`, auth.HashToken(token)).Scan(&session, &orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", connect.NewError(connect.CodeUnauthenticated, errors.New("the sandbox token is unknown or its sandbox is gone"))
	}
	if err != nil {
		return "", "", Internal(err)
	}
	return session, orgID, nil
}

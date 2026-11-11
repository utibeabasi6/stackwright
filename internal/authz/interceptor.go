package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
)

type Scope struct {
	Tx     pgx.Tx
	UserID string
	OrgID  string
	Role   v1.Role
	IP     string
	target string
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

type Interceptor struct {
	Pool *pgxpool.Pool
}

func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		procedure := req.Spec().Procedure
		rule, ok := Policy[procedure]
		if !ok {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s has no policy", procedure))
		}
		user, signedIn := auth.UserFrom(ctx)
		if rule.Access != Public && !signedIn {
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
		case InstanceAdmin:
			var admin bool
			if err := tx.QueryRow(ctx, `SELECT instance_admin FROM users WHERE id = $1`, user.ID).Scan(&admin); err != nil {
				return nil, Internal(err)
			}
			if !admin {
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only an instance admin may do this"))
			}
		case InOrg:
			msg, ok := req.Any().(interface{ GetOrg() string })
			if !ok {
				return nil, Internal(fmt.Errorf("%s is org-scoped but its request has no org", procedure))
			}
			var orgID, role string
			err := tx.QueryRow(ctx, `SELECT o.id, m.role FROM orgs o JOIN memberships m ON m.org_id = o.id
				WHERE o.slug = $1 AND m.user_id = app_user_id()`, msg.GetOrg()).Scan(&orgID, &role)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
			}
			if err != nil {
				return nil, Internal(err)
			}
			s.Role = RoleFromDB(role)
			if !AtLeast(s.Role, rule.Min) {
				return nil, connect.NewError(connect.CodePermissionDenied,
					fmt.Errorf("this needs the %s role or above", RoleToDB(rule.Min)))
			}
			if err := s.SetOrg(ctx, orgID); err != nil {
				return nil, Internal(err)
			}
		}

		resp, err := next(context.WithValue(ctx, scopeKey{}, s), req)
		if err != nil {
			return nil, err
		}
		if req.Spec().IdempotencyLevel != connect.IdempotencyNoSideEffects {
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
		return resp, nil
	}
}

func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *Interceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(_ context.Context, conn connect.StreamingHandlerConn) error {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s: streaming has no policy yet", conn.Spec().Procedure))
	}
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

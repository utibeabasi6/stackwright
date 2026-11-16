package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/authz"
)

type Auth struct {
	stackwrightv1connect.UnimplementedAuthServiceHandler
	Cookies   auth.Cookies
	SetupCode string

	mu       sync.Mutex
	failures map[string][]time.Time
	swept    time.Time
}

// Failed sign-ins are limited per email from one address, so a guesser at
// one address cannot lock the account out for everyone, and per email from
// all addresses, so many addresses cannot guess freely.
const (
	signInLimit      = 10
	signInEmailLimit = 100
	signInWindow     = 15 * time.Minute
)

// recent drops key's failures older than the window and returns the rest.
// a.mu must be held.
func (a *Auth) recent(key string, now time.Time) []time.Time {
	kept := a.failures[key][:0]
	for _, t := range a.failures[key] {
		if now.Sub(t) < signInWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.failures, key)
	} else {
		a.failures[key] = kept
	}
	return kept
}

func (a *Auth) throttled(email, ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.Sub(a.swept) > time.Minute {
		for key := range a.failures {
			a.recent(key, now)
		}
		a.swept = now
	}
	email = strings.ToLower(email)
	return len(a.recent(email+" "+ip, now)) >= signInLimit || len(a.recent(email, now)) >= signInEmailLimit
}

func (a *Auth) failed(email, ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failures == nil {
		a.failures = map[string][]time.Time{}
	}
	email, now := strings.ToLower(email), time.Now()
	a.failures[email+" "+ip] = append(a.failures[email+" "+ip], now)
	a.failures[email] = append(a.failures[email], now)
}

func (a *Auth) startSession(ctx context.Context, s *authz.Scope, h interface{ Header() http.Header }, userID string) error {
	token, csrf, err := auth.CreateSession(ctx, s.Tx, userID)
	if err != nil {
		return authz.Internal(err)
	}
	a.Cookies.Set(h.Header(), token, csrf)
	return nil
}

func (a *Auth) GetSetup(ctx context.Context, _ *connect.Request[v1.GetSetupRequest]) (*connect.Response[v1.GetSetupResponse], error) {
	var needed bool
	if err := authz.ScopeFrom(ctx).Tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT FROM users)`).Scan(&needed); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.GetSetupResponse{Needed: needed}), nil
}

func (a *Auth) CompleteSetup(ctx context.Context, req *connect.Request[v1.CompleteSetupRequest]) (*connect.Response[v1.CompleteSetupResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if _, err := s.Tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('stackwright.setup'))`); err != nil {
		return nil, authz.Internal(err)
	}
	var done bool
	if err := s.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM users)`).Scan(&done); err != nil {
		return nil, authz.Internal(err)
	}
	if done || a.SetupCode == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this instance is already set up"))
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(m.SetupCode)), []byte(a.SetupCode)) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("wrong setup code"))
	}
	if err := checkOrg(m.OrgSlug, m.OrgName); err != nil {
		return nil, err
	}
	if err := checkEmail(m.Email); err != nil {
		return nil, err
	}
	if err := auth.CheckStrength(m.Password); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	userID, orgID := uuid.NewString(), uuid.NewString()
	if _, err := s.Tx.Exec(ctx, `INSERT INTO users (id, email, name, password_hash, instance_admin) VALUES ($1, $2, $3, $4, true)`,
		userID, m.Email, auth.ClipName(m.Name), auth.HashPassword(m.Password)); err != nil {
		return nil, authz.Internal(err)
	}
	if err := s.SetUser(ctx, userID); err != nil {
		return nil, authz.Internal(err)
	}
	if err := s.SetOrg(ctx, orgID); err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`, orgID, m.OrgSlug, strings.TrimSpace(m.OrgName)); err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, userID); err != nil {
		return nil, authz.Internal(err)
	}
	resp := connect.NewResponse(&v1.CompleteSetupResponse{OrgSlug: m.OrgSlug})
	if err := a.startSession(ctx, s, resp, userID); err != nil {
		return nil, err
	}
	s.Target("org:" + m.OrgSlug)
	return resp, nil
}

var errWrongPassword = connect.NewError(connect.CodeUnauthenticated, errors.New("wrong email or password"))

func (a *Auth) SignIn(ctx context.Context, req *connect.Request[v1.SignInRequest]) (*connect.Response[v1.SignInResponse], error) {
	s := authz.ScopeFrom(ctx)
	if len(req.Msg.Email) > 254 {
		return nil, errWrongPassword
	}
	if a.throttled(req.Msg.Email, s.IP) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many sign-in attempts for this email; wait 15 minutes"))
	}
	var userID, hash string
	err := s.Tx.QueryRow(ctx, `SELECT id, coalesce(password_hash, '') FROM users WHERE lower(email) = lower($1)`,
		req.Msg.Email).Scan(&userID, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, authz.Internal(err)
	}
	if !auth.PasswordMatches(hash, req.Msg.Password) {
		a.failed(req.Msg.Email, s.IP)
		return nil, errWrongPassword
	}
	if err := s.SetUser(ctx, userID); err != nil {
		return nil, authz.Internal(err)
	}
	resp := connect.NewResponse(&v1.SignInResponse{})
	if err := a.startSession(ctx, s, resp, userID); err != nil {
		return nil, err
	}
	s.Target("user:" + userID)
	return resp, nil
}

func (a *Auth) SignOut(ctx context.Context, req *connect.Request[v1.SignOutRequest]) (*connect.Response[v1.SignOutResponse], error) {
	s := authz.ScopeFrom(ctx)
	if _, err := s.Tx.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, auth.HashToken(auth.SessionToken(req.Header()))); err != nil {
		return nil, authz.Internal(err)
	}
	resp := connect.NewResponse(&v1.SignOutResponse{})
	a.Cookies.Set(resp.Header(), "", "")
	s.Target("user:" + s.UserID)
	return resp, nil
}

func (a *Auth) GetMe(ctx context.Context, _ *connect.Request[v1.GetMeRequest]) (*connect.Response[v1.GetMeResponse], error) {
	s := authz.ScopeFrom(ctx)
	u := &v1.User{Id: s.UserID}
	if err := s.Tx.QueryRow(ctx, `SELECT email, name, instance_admin FROM users WHERE id = $1`, s.UserID).
		Scan(&u.Email, &u.Name, &u.InstanceAdmin); err != nil {
		return nil, authz.Internal(err)
	}
	// A session from an org's SAML sign-in shows only that org (adr/0045).
	user, _ := auth.UserFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT o.slug, o.name, m.role, coalesce(sso.required, false) AND m.role <> 'owner' AND o.id::text <> $2
		FROM memberships m JOIN orgs o ON o.id = m.org_id LEFT JOIN org_saml sso ON sso.org_id = o.id
		WHERE m.user_id = $1 AND ($2 = '' OR o.id::text = $2) ORDER BY o.name`, s.UserID, user.SSOOrg)
	if err != nil {
		return nil, authz.Internal(err)
	}
	var orgs []*v1.OrgMembership
	for rows.Next() {
		o := &v1.OrgMembership{}
		var role string
		if err := rows.Scan(&o.Slug, &o.Name, &role, &o.NeedsSso); err != nil {
			return nil, authz.Internal(err)
		}
		o.Role = authz.RoleFromDB(role)
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.GetMeResponse{User: u, Orgs: orgs}), nil
}

func (a *Auth) ListSignInProviders(ctx context.Context, _ *connect.Request[v1.ListSignInProvidersRequest]) (*connect.Response[v1.ListSignInProvidersResponse], error) {
	rows, err := authz.ScopeFrom(ctx).Tx.Query(ctx, `SELECT id, name FROM sign_in_providers ORDER BY name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.SignInProviderButton, error) {
		p := &v1.SignInProviderButton{}
		return p, r.Scan(&p.Id, &p.Name)
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListSignInProvidersResponse{Providers: ps}), nil
}

func inviteError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInviteNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, auth.ErrInviteEmail):
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return authz.Internal(err)
}

func (a *Auth) GetInvite(ctx context.Context, req *connect.Request[v1.GetInviteRequest]) (*connect.Response[v1.GetInviteResponse], error) {
	s := authz.ScopeFrom(ctx)
	inv, err := auth.LookupInvite(ctx, s.Tx, req.Msg.Token)
	if err != nil {
		return nil, inviteError(err)
	}
	out := &v1.GetInviteResponse{OrgName: inv.OrgName, Email: inv.Email, Role: authz.RoleFromDB(inv.Role)}
	if err := s.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM users WHERE lower(email) = lower($1))`, inv.Email).Scan(&out.HasAccount); err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(out), nil
}

func (a *Auth) AcceptInvite(ctx context.Context, req *connect.Request[v1.AcceptInviteRequest]) (*connect.Response[v1.AcceptInviteResponse], error) {
	s := authz.ScopeFrom(ctx)
	inv, err := auth.LookupInvite(ctx, s.Tx, req.Msg.Token)
	if err != nil {
		return nil, inviteError(err)
	}
	resp := connect.NewResponse(&v1.AcceptInviteResponse{OrgSlug: inv.OrgSlug})
	var email string
	if s.UserID != "" {
		if err := s.Tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, s.UserID).Scan(&email); err != nil {
			return nil, authz.Internal(err)
		}
	} else {
		var exists bool
		if err := s.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM users WHERE lower(email) = lower($1))`, inv.Email).Scan(&exists); err != nil {
			return nil, authz.Internal(err)
		}
		if exists {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("you have an account; sign in, then open the invite link again"))
		}
		if err := auth.CheckStrength(req.Msg.Password); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		userID := uuid.NewString()
		email = inv.Email
		if _, err := s.Tx.Exec(ctx, `INSERT INTO users (id, email, name, password_hash) VALUES ($1, $2, $3, $4)`,
			userID, email, auth.ClipName(req.Msg.Name), auth.HashPassword(req.Msg.Password)); err != nil {
			return nil, authz.Internal(err)
		}
		if err := s.SetUser(ctx, userID); err != nil {
			return nil, authz.Internal(err)
		}
		if err := a.startSession(ctx, s, resp, userID); err != nil {
			return nil, err
		}
	}
	if err := auth.AcceptInvite(ctx, s.Tx, inv, s.UserID, email); err != nil {
		return nil, inviteError(err)
	}
	s.OrgID = inv.OrgID
	s.Target("invite:" + inv.ID)
	return resp, nil
}

func deviceByCode(ctx context.Context, tx pgx.Tx, code string) (string, time.Time, error) {
	var name string
	var created time.Time
	err := tx.QueryRow(ctx, `SELECT name, created_at FROM device_codes WHERE user_code = $1 AND expires_at > now()
		AND approved_by IS NULL AND NOT denied`, auth.UserCode(code)).Scan(&name, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", created, connect.NewError(connect.CodeNotFound, errors.New("no sign-in waits for that code; it may have ended after 10 minutes"))
	}
	if err != nil {
		return "", created, authz.Internal(err)
	}
	return name, created, nil
}

func (a *Auth) GetDevice(ctx context.Context, req *connect.Request[v1.GetDeviceRequest]) (*connect.Response[v1.GetDeviceResponse], error) {
	name, created, err := deviceByCode(ctx, authz.ScopeFrom(ctx).Tx, req.Msg.UserCode)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetDeviceResponse{Name: name, CreatedAt: timestamppb.New(created)}), nil
}

func (a *Auth) decideDevice(ctx context.Context, code string, approve bool) error {
	s := authz.ScopeFrom(ctx)
	name, _, err := deviceByCode(ctx, s.Tx, code)
	if err != nil {
		return err
	}
	if approve {
		u, _ := auth.UserFrom(ctx)
		_, err = s.Tx.Exec(ctx, `UPDATE device_codes SET approved_by = $2, sso_org_id = nullif($3, '')::uuid WHERE user_code = $1`,
			auth.UserCode(code), s.UserID, u.SSOOrg)
	} else {
		_, err = s.Tx.Exec(ctx, `UPDATE device_codes SET denied = true WHERE user_code = $1`, auth.UserCode(code))
	}
	if err != nil {
		return authz.Internal(err)
	}
	s.Target("cli sign-in of " + name)
	return nil
}

func (a *Auth) ApproveDevice(ctx context.Context, req *connect.Request[v1.ApproveDeviceRequest]) (*connect.Response[v1.ApproveDeviceResponse], error) {
	if err := a.decideDevice(ctx, req.Msg.UserCode, true); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ApproveDeviceResponse{}), nil
}

func (a *Auth) DenyDevice(ctx context.Context, req *connect.Request[v1.DenyDeviceRequest]) (*connect.Response[v1.DenyDeviceResponse], error) {
	if err := a.decideDevice(ctx, req.Msg.UserCode, false); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.DenyDeviceResponse{}), nil
}

func (a *Auth) ListCliTokens(ctx context.Context, _ *connect.Request[v1.ListCliTokensRequest]) (*connect.Response[v1.ListCliTokensResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT id, name, created_at, last_used_at FROM cli_tokens WHERE user_id = $1 AND revoked_at IS NULL
		AND last_used_at > $2 ORDER BY last_used_at DESC`, s.UserID, time.Now().Add(-auth.CLITokenIdle))
	if err != nil {
		return nil, authz.Internal(err)
	}
	tokens, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.CliToken, error) {
		t := &v1.CliToken{}
		var created, used time.Time
		err := r.Scan(&t.Id, &t.Name, &created, &used)
		t.CreatedAt, t.LastUsedAt = timestamppb.New(created), timestamppb.New(used)
		return t, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListCliTokensResponse{Tokens: tokens}), nil
}

func (a *Auth) RevokeCliToken(ctx context.Context, req *connect.Request[v1.RevokeCliTokenRequest]) (*connect.Response[v1.RevokeCliTokenResponse], error) {
	s := authz.ScopeFrom(ctx)
	if _, err := uuid.Parse(req.Msg.Id); err != nil {
		return nil, errNotFound
	}
	tag, err := s.Tx.Exec(ctx, `UPDATE cli_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, req.Msg.Id, s.UserID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	s.Target("cli token " + req.Msg.Id)
	return connect.NewResponse(&v1.RevokeCliTokenResponse{}), nil
}

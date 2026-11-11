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

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
)

type Auth struct {
	stackwrightv1connect.UnimplementedAuthServiceHandler
	Cookies   auth.Cookies
	SetupCode string

	mu       sync.Mutex
	attempts map[string][]time.Time
}

const (
	signInLimit  = 10
	signInWindow = 15 * time.Minute
)

func (a *Auth) allowSignIn(email string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attempts == nil {
		a.attempts = map[string][]time.Time{}
	}
	key := strings.ToLower(email)
	cutoff := time.Now().Add(-signInWindow)
	recent := a.attempts[key][:0]
	for _, t := range a.attempts[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= signInLimit {
		a.attempts[key] = recent
		return false
	}
	a.attempts[key] = append(recent, time.Now())
	return true
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
		userID, m.Email, strings.TrimSpace(m.Name), auth.HashPassword(m.Password)); err != nil {
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
	if !a.allowSignIn(req.Msg.Email) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many sign-in attempts for this email; wait 15 minutes"))
	}
	var userID, hash string
	err := s.Tx.QueryRow(ctx, `SELECT id, coalesce(password_hash, '') FROM users WHERE lower(email) = lower($1)`,
		req.Msg.Email).Scan(&userID, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, authz.Internal(err)
	}
	if !auth.PasswordMatches(hash, req.Msg.Password) {
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
	rows, err := s.Tx.Query(ctx, `SELECT o.slug, o.name, m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = $1 ORDER BY o.name`, s.UserID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	var orgs []*v1.OrgMembership
	for rows.Next() {
		o := &v1.OrgMembership{}
		var role string
		if err := rows.Scan(&o.Slug, &o.Name, &role); err != nil {
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
			userID, email, strings.TrimSpace(req.Msg.Name), auth.HashPassword(req.Msg.Password)); err != nil {
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

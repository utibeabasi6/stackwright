package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/authz"
	"github.com/utibeabasi6/stackwright/internal/db"
)

const inviteTTL = 7 * 24 * time.Hour

type Orgs struct {
	stackwrightv1connect.UnimplementedOrgServiceHandler
	PublicURL string
}

var errNotFound = connect.NewError(connect.CodeNotFound, errors.New("not found"))

func (o *Orgs) CreateOrg(ctx context.Context, req *connect.Request[v1.CreateOrgRequest]) (*connect.Response[v1.CreateOrgResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := checkOrg(req.Msg.Slug, req.Msg.Name); err != nil {
		return nil, err
	}
	orgID := uuid.NewString()
	if err := s.SetOrg(ctx, orgID); err != nil {
		return nil, authz.Internal(err)
	}
	_, err := s.Tx.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`, orgID, req.Msg.Slug, strings.TrimSpace(req.Msg.Name))
	if db.UniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("an org with that short name exists"))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, s.UserID); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("org:" + req.Msg.Slug)
	return connect.NewResponse(&v1.CreateOrgResponse{Slug: req.Msg.Slug}), nil
}

func (o *Orgs) ListMembers(ctx context.Context, _ *connect.Request[v1.ListMembersRequest]) (*connect.Response[v1.ListMembersResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT u.id, u.email, u.name, m.role, m.created_at FROM memberships m
		JOIN users u ON u.id = m.user_id WHERE m.org_id = app_org_id() ORDER BY lower(u.email)`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Member, error) {
		m := &v1.Member{}
		var role string
		var joined time.Time
		err := r.Scan(&m.UserId, &m.Email, &m.Name, &role, &joined)
		m.Role, m.JoinedAt = authz.RoleFromDB(role), timestamppb.New(joined)
		return m, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListMembersResponse{Members: members}), nil
}

func memberRole(ctx context.Context, s *authz.Scope, userID string) (v1.Role, error) {
	if _, err := uuid.Parse(userID); err != nil {
		return 0, errNotFound
	}
	var role string
	err := s.Tx.QueryRow(ctx, `SELECT role FROM memberships WHERE org_id = app_org_id() AND user_id = $1 FOR UPDATE`, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	if err != nil {
		return 0, authz.Internal(err)
	}
	return authz.RoleFromDB(role), nil
}

func lastOwner(ctx context.Context, s *authz.Scope) (bool, error) {
	// Locking the owners makes two demotions at once count in turn, so
	// neither leaves the org with none.
	rows, err := s.Tx.Query(ctx, `SELECT user_id FROM memberships WHERE org_id = app_org_id() AND role = 'owner' FOR UPDATE`)
	if err != nil {
		return false, authz.Internal(err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, authz.Internal(err)
	}
	return len(owners) <= 1, nil
}

var errOwnersOnly = connect.NewError(connect.CodePermissionDenied, errors.New("only an owner may give or take the owner role"))

func (o *Orgs) SetMemberRole(ctx context.Context, req *connect.Request[v1.SetMemberRoleRequest]) (*connect.Response[v1.SetMemberRoleResponse], error) {
	s := authz.ScopeFrom(ctx)
	if !authz.ValidRole(req.Msg.Role) {
		return nil, invalid("unknown role")
	}
	current, err := memberRole(ctx, s, req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	if (current == v1.Role_ROLE_OWNER || req.Msg.Role == v1.Role_ROLE_OWNER) && s.Role != v1.Role_ROLE_OWNER {
		return nil, errOwnersOnly
	}
	if current == v1.Role_ROLE_OWNER && req.Msg.Role != v1.Role_ROLE_OWNER {
		if last, err := lastOwner(ctx, s); err != nil || last {
			return nil, cmpErr(err, connect.NewError(connect.CodeFailedPrecondition, errors.New("an org needs at least one owner")))
		}
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE memberships SET role = $2 WHERE org_id = app_org_id() AND user_id = $1`,
		req.Msg.UserId, authz.RoleToDB(req.Msg.Role)); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("member:" + req.Msg.UserId)
	return connect.NewResponse(&v1.SetMemberRoleResponse{}), nil
}

func (o *Orgs) RemoveMember(ctx context.Context, req *connect.Request[v1.RemoveMemberRequest]) (*connect.Response[v1.RemoveMemberResponse], error) {
	s := authz.ScopeFrom(ctx)
	current, err := memberRole(ctx, s, req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	if current == v1.Role_ROLE_OWNER {
		if s.Role != v1.Role_ROLE_OWNER {
			return nil, errOwnersOnly
		}
		if last, err := lastOwner(ctx, s); err != nil || last {
			return nil, cmpErr(err, connect.NewError(connect.CodeFailedPrecondition, errors.New("an org needs at least one owner")))
		}
	}
	if _, err := s.Tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = app_org_id() AND user_id = $1`, req.Msg.UserId); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("member:" + req.Msg.UserId)
	return connect.NewResponse(&v1.RemoveMemberResponse{}), nil
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

func (o *Orgs) CreateInvite(ctx context.Context, req *connect.Request[v1.CreateInviteRequest]) (*connect.Response[v1.CreateInviteResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if err := checkEmail(m.Email); err != nil {
		return nil, err
	}
	if !authz.ValidRole(m.Role) {
		return nil, invalid("unknown role")
	}
	if m.Role == v1.Role_ROLE_OWNER && s.Role != v1.Role_ROLE_OWNER {
		return nil, errOwnersOnly
	}
	token, id, expires := auth.RandomToken(), uuid.NewString(), time.Now().Add(inviteTTL)
	_, err := s.Tx.Exec(ctx, `INSERT INTO invites (id, org_id, email, role, token_hash, invited_by, expires_at)
		VALUES ($1, app_org_id(), $2, $3, $4, $5, $6)`, id, m.Email, authz.RoleToDB(m.Role), auth.HashToken(token), s.UserID, expires)
	if db.UniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("an open invite for this email exists; revoke it first"))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("invite:" + id)
	return connect.NewResponse(&v1.CreateInviteResponse{
		Invite: &v1.Invite{Id: id, Email: m.Email, Role: m.Role, ExpiresAt: timestamppb.New(expires)},
		Url:    o.PublicURL + "/invite/" + token,
	}), nil
}

func (o *Orgs) ListInvites(ctx context.Context, _ *connect.Request[v1.ListInvitesRequest]) (*connect.Response[v1.ListInvitesResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT i.id, i.email, i.role, coalesce(nullif(u.name, ''), u.email), i.expires_at
		FROM invites i JOIN users u ON u.id = i.invited_by
		WHERE i.org_id = app_org_id() AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
		ORDER BY i.created_at DESC`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	invites, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Invite, error) {
		i := &v1.Invite{}
		var role string
		var expires time.Time
		err := r.Scan(&i.Id, &i.Email, &role, &i.InvitedBy, &expires)
		i.Role, i.ExpiresAt = authz.RoleFromDB(role), timestamppb.New(expires)
		return i, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListInvitesResponse{Invites: invites}), nil
}

func (o *Orgs) RevokeInvite(ctx context.Context, req *connect.Request[v1.RevokeInviteRequest]) (*connect.Response[v1.RevokeInviteResponse], error) {
	s := authz.ScopeFrom(ctx)
	if _, err := uuid.Parse(req.Msg.Id); err != nil {
		return nil, errNotFound
	}
	tag, err := s.Tx.Exec(ctx, `UPDATE invites SET revoked_at = now()
		WHERE id = $1 AND org_id = app_org_id() AND accepted_at IS NULL AND revoked_at IS NULL`, req.Msg.Id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	s.Target("invite:" + req.Msg.Id)
	return connect.NewResponse(&v1.RevokeInviteResponse{}), nil
}

func (o *Orgs) ListAuditEvents(ctx context.Context, req *connect.Request[v1.ListAuditEventsRequest]) (*connect.Response[v1.ListAuditEventsResponse], error) {
	s := authz.ScopeFrom(ctx)
	before := req.Msg.BeforeId
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.Tx.Query(ctx, `SELECT a.id, coalesce(nullif(u.name, ''), u.email, a.actor_id), a.action, a.target, a.at
		FROM audit_log a LEFT JOIN users u ON a.actor_kind = 'user' AND u.id::text = a.actor_id
		WHERE a.org_id = app_org_id() AND a.id < $1 ORDER BY a.id DESC LIMIT 50`, before)
	if err != nil {
		return nil, authz.Internal(err)
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.AuditEvent, error) {
		e := &v1.AuditEvent{}
		var at time.Time
		err := r.Scan(&e.Id, &e.Actor, &e.Action, &e.Target, &at)
		e.At = timestamppb.New(at)
		return e, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListAuditEventsResponse{Events: events}), nil
}

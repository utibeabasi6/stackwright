package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/utibeabasi6/stackwright/internal/state"
)

type State struct {
	stackwrightv1connect.UnimplementedStateServiceHandler
	Store     state.Store
	PublicURL string
}

func workspaceID(ctx context.Context, tx pgx.Tx, project, workspace string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT w.id FROM workspaces w JOIN projects p ON p.id = w.project_id
		WHERE p.slug = $1 AND w.name = $2 AND p.archived_at IS NULL AND w.archived_at IS NULL`, project, workspace).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", authz.Internal(err)
	}
	return id, nil
}

const versionColumns = `v.id, v.serial, v.resources, v.size, coalesce(nullif(u.name, ''), u.email, ''), v.created_at, coalesce(r.serial, 0)
	FROM state_versions v LEFT JOIN users u ON u.id = v.created_by LEFT JOIN state_versions r ON r.id = v.rolled_back_from`

func scanVersion(r pgx.Row) (*v1.StateVersion, error) {
	v := &v1.StateVersion{}
	var at time.Time
	err := r.Scan(&v.Id, &v.Serial, &v.Resources, &v.Size, &v.CreatedBy, &at, &v.RolledBackFromSerial)
	v.CreatedAt = timestamppb.New(at)
	return v, err
}

func (s *State) GetState(ctx context.Context, req *connect.Request[v1.GetStateRequest]) (*connect.Response[v1.GetStateResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	ws, err := workspaceID(ctx, sc.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	address := s.PublicURL + "/state/" + m.Org + "/" + m.Project + "/" + m.Workspace
	out := &v1.GetStateResponse{
		Address: address,
		BackendConfig: fmt.Sprintf("address        = %q\nlock_address   = %q\nunlock_address = %q\nusername       = \"stackwright\"\n",
			address, address, address),
	}
	latest, err := scanVersion(sc.Tx.QueryRow(ctx, `SELECT `+versionColumns+` WHERE v.workspace_id = $1 ORDER BY v.serial DESC LIMIT 1`, ws))
	switch {
	case err == nil:
		out.Latest = latest
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, authz.Internal(err)
	}
	var info []byte
	l := &v1.StateLock{}
	var at time.Time
	err = sc.Tx.QueryRow(ctx, `SELECT l.lock_id, l.info, coalesce(nullif(u.name, ''), u.email, ''), l.created_at
		FROM state_locks l LEFT JOIN users u ON u.id = l.locked_by WHERE l.workspace_id = $1`, ws).Scan(&l.Id, &info, &l.LockedBy, &at)
	switch {
	case err == nil:
		var li struct{ Operation, Who string }
		_ = json.Unmarshal(info, &li)
		l.Operation, l.Who, l.CreatedAt = li.Operation, li.Who, timestamppb.New(at)
		out.Lock = l
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(out), nil
}

func (s *State) ListStateVersions(ctx context.Context, req *connect.Request[v1.ListStateVersionsRequest]) (*connect.Response[v1.ListStateVersionsResponse], error) {
	sc := authz.ScopeFrom(ctx)
	ws, err := workspaceID(ctx, sc.Tx, req.Msg.Project, req.Msg.Workspace)
	if err != nil {
		return nil, err
	}
	rows, err := sc.Tx.Query(ctx, `SELECT `+versionColumns+` WHERE v.workspace_id = $1 ORDER BY v.serial DESC LIMIT 100`, ws)
	if err != nil {
		return nil, authz.Internal(err)
	}
	versions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.StateVersion, error) { return scanVersion(r) })
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListStateVersionsResponse{Versions: versions}), nil
}

func (s *State) GetStateVersion(ctx context.Context, req *connect.Request[v1.GetStateVersionRequest]) (*connect.Response[v1.GetStateVersionResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	ws, err := workspaceID(ctx, sc.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(m.Id); err != nil {
		return nil, errNotFound
	}
	v, err := scanVersion(sc.Tx.QueryRow(ctx, `SELECT `+versionColumns+` WHERE v.workspace_id = $1 AND v.id = $2`, ws, m.Id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	var blobKey string
	if err := sc.Tx.QueryRow(ctx, `SELECT blob_key FROM state_versions WHERE id = $1`, m.Id).Scan(&blobKey); err != nil {
		return nil, authz.Internal(err)
	}
	body, err := s.Store.Read(ctx, sc.Tx, sc.OrgID, blobKey)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.GetStateVersionResponse{Version: v, State: string(body)}), nil
}

func (s *State) RollbackState(ctx context.Context, req *connect.Request[v1.RollbackStateRequest]) (*connect.Response[v1.RollbackStateResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	ws, err := workspaceID(ctx, sc.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(m.Id); err != nil {
		return nil, errNotFound
	}
	var lockedBy string
	switch err := sc.Tx.QueryRow(ctx, `SELECT lock_id FROM state_locks WHERE workspace_id = $1`, ws).Scan(&lockedBy); {
	case err == nil:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the state is locked; wait for the run to finish, or force-unlock it first"))
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, authz.Internal(err)
	}
	nv, err := s.Store.Rollback(ctx, sc.Tx, sc.OrgID, ws, sc.UserID, m.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	v, err := scanVersion(sc.Tx.QueryRow(ctx, `SELECT `+versionColumns+` WHERE v.id = $1`, nv.ID))
	if err != nil {
		return nil, authz.Internal(err)
	}
	sc.Target(fmt.Sprintf("workspace:%s/%s@%d", m.Project, m.Workspace, v.Serial))
	return connect.NewResponse(&v1.RollbackStateResponse{Version: v}), nil
}

func (s *State) ForceUnlockState(ctx context.Context, req *connect.Request[v1.ForceUnlockStateRequest]) (*connect.Response[v1.ForceUnlockStateResponse], error) {
	sc := authz.ScopeFrom(ctx)
	m := req.Msg
	ws, err := workspaceID(ctx, sc.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	tag, err := sc.Tx.Exec(ctx, `DELETE FROM state_locks WHERE workspace_id = $1`, ws)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the state is not locked"))
	}
	sc.Target("workspace:" + m.Project + "/" + m.Workspace)
	return connect.NewResponse(&v1.ForceUnlockStateResponse{}), nil
}

type Tokens struct {
	stackwrightv1connect.UnimplementedTokenServiceHandler
}

func (Tokens) ListApiTokens(ctx context.Context, _ *connect.Request[v1.ListApiTokensRequest]) (*connect.Response[v1.ListApiTokensResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT id, name, created_at, expires_at, last_used_at FROM api_tokens
		WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL AND expires_at > now() ORDER BY created_at DESC`, s.UserID, s.OrgID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	tokens, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.ApiToken, error) {
		t := &v1.ApiToken{}
		var created, expires time.Time
		var used *time.Time
		err := r.Scan(&t.Id, &t.Name, &created, &expires, &used)
		t.CreatedAt, t.ExpiresAt = timestamppb.New(created), timestamppb.New(expires)
		if used != nil {
			t.LastUsedAt = timestamppb.New(*used)
		}
		return t, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListApiTokensResponse{Tokens: tokens}), nil
}

func (Tokens) CreateApiToken(ctx context.Context, req *connect.Request[v1.CreateApiTokenRequest]) (*connect.Response[v1.CreateApiTokenResponse], error) {
	s := authz.ScopeFrom(ctx)
	name := strings.TrimSpace(req.Msg.Name)
	days := req.Msg.ExpiresInDays
	if days == 0 {
		days = 90
	}
	switch {
	case name == "" || len(name) > 60:
		return nil, invalid("the token's name needs 1 to 60 characters")
	case days < 1 || days > 365:
		return nil, invalid("a token lasts 1 to 365 days")
	}
	secret, id := auth.NewAPIToken(), uuid.NewString()
	created, expires := time.Now(), time.Now().Add(time.Duration(days)*24*time.Hour)
	if _, err := s.Tx.Exec(ctx, `INSERT INTO api_tokens (id, user_id, org_id, name, token_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, s.UserID, s.OrgID, name, auth.HashToken(secret), created, expires); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("api_token:" + id)
	return connect.NewResponse(&v1.CreateApiTokenResponse{
		Token:  &v1.ApiToken{Id: id, Name: name, CreatedAt: timestamppb.New(created), ExpiresAt: timestamppb.New(expires)},
		Secret: secret,
	}), nil
}

func (Tokens) RevokeApiToken(ctx context.Context, req *connect.Request[v1.RevokeApiTokenRequest]) (*connect.Response[v1.RevokeApiTokenResponse], error) {
	s := authz.ScopeFrom(ctx)
	if _, err := uuid.Parse(req.Msg.Id); err != nil {
		return nil, errNotFound
	}
	tag, err := s.Tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND org_id = $3 AND revoked_at IS NULL`, req.Msg.Id, s.UserID, s.OrgID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	s.Target("api_token:" + req.Msg.Id)
	return connect.NewResponse(&v1.RevokeApiTokenResponse{}), nil
}

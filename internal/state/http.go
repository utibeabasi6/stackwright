package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/internal/auth"
)

type Handler struct {
	Pool  *pgxpool.Pool
	Store Store
}

type lockInfo struct {
	ID string `json:"ID"`
}

type httpError struct {
	status int
	msg    string
	body   []byte
}

func (e *httpError) Error() string { return e.msg }

func fail(status int, msg string) error { return &httpError{status: status, msg: msg} }

var errNotFound = fail(http.StatusNotFound, "no such workspace, or your token cannot reach it")

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := h.serve(w, r)
	if err == nil {
		return
	}
	var he *httpError
	if !errors.As(err, &he) {
		slog.Error("state request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		he = &httpError{status: http.StatusInternalServerError, msg: "internal error"}
	}
	if he.body != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(he.status)
		_, _ = w.Write(he.body)
		return
	}
	http.Error(w, he.msg, he.status)
}

func (h Handler) serve(w http.ResponseWriter, r *http.Request) error {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/state/"), "/")
	if len(parts) != 3 {
		return errNotFound
	}
	org, project, workspace := parts[0], parts[1], parts[2]

	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, "LOCK", "UNLOCK":
	case http.MethodDelete:
		return fail(http.StatusMethodNotAllowed, "Stackwright keeps state history; archive the workspace in Stackwright instead")
	default:
		return fail(http.StatusMethodNotAllowed, "method not allowed")
	}

	_, token, ok := r.BasicAuth()
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="stackwright"`)
		return fail(http.StatusUnauthorized, "set TF_HTTP_PASSWORD to a Stackwright API token")
	}
	userID, tokenOrg, err := auth.LookupToken(r.Context(), h.Pool, token)
	if errors.Is(err, auth.ErrBadToken) {
		return fail(http.StatusUnauthorized, err.Error())
	}
	if err != nil {
		return err
	}

	var body []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSize))
		if err != nil {
			return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("state is larger than %d MiB", MaxSize>>20))
		}
	}

	return pgx.BeginFunc(r.Context(), h.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `SELECT set_config('stackwright.user_id', $1, true)`, userID); err != nil {
			return err
		}
		var orgID, role string
		err := tx.QueryRow(r.Context(), `SELECT o.id, m.role FROM orgs o JOIN memberships m ON m.org_id = o.id
			WHERE o.slug = $1 AND m.user_id = app_user_id()`, org).Scan(&orgID, &role)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && orgID != tokenOrg) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if write && (role == "viewer" || role == "security" || role == "reviewer") {
			return fail(http.StatusForbidden, "writing state needs the writer role or above")
		}
		if _, err := tx.Exec(r.Context(), `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		var workspaceID string
		err = tx.QueryRow(r.Context(), `SELECT w.id FROM workspaces w JOIN projects p ON p.id = w.project_id
			WHERE p.slug = $1 AND w.name = $2 AND p.archived_at IS NULL AND w.archived_at IS NULL`,
			project, workspace).Scan(&workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		req := request{ctx: r.Context(), tx: tx, orgID: orgID, workspaceID: workspaceID, userID: userID,
			target: "workspace:" + project + "/" + workspace, ip: auth.ClientIP(r)}

		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return h.get(w, r, req)
		case http.MethodPost:
			return h.post(w, r, req, body)
		case "LOCK":
			return h.lock(w, req, body)
		default:
			return h.unlock(w, req, body)
		}
	})
}

type request struct {
	ctx                                context.Context
	tx                                 pgx.Tx
	orgID, workspaceID, userID, target string
	ip                                 string
}

func (h Handler) get(w http.ResponseWriter, r *http.Request, q request) error {
	v, ok, err := Latest(q.ctx, q.tx, q.workspaceID)
	if err != nil {
		return err
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	body, err := h.Store.Read(q.ctx, q.tx, q.orgID, v.BlobKey)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = w.Write(body)
	return err
}

func currentLock(q request) (lockInfo, []byte, bool, error) {
	var raw []byte
	var l lockInfo
	err := q.tx.QueryRow(q.ctx, `SELECT info FROM state_locks WHERE workspace_id = $1 FOR UPDATE`, q.workspaceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockInfo{}, nil, false, nil
	}
	if err != nil {
		return lockInfo{}, nil, false, err
	}
	_ = json.Unmarshal(raw, &l)
	return l, raw, true, nil
}

func locked(raw []byte) error {
	return &httpError{status: http.StatusLocked, msg: "locked", body: raw}
}

func (h Handler) post(w http.ResponseWriter, r *http.Request, q request, body []byte) error {
	l, raw, isLocked, err := currentLock(q)
	if err != nil {
		return err
	}
	if isLocked && r.URL.Query().Get("ID") != l.ID {
		return locked(raw)
	}
	v, err := h.Store.Write(q.ctx, q.tx, q.orgID, q.workspaceID, q.userID, body)
	switch {
	case errors.Is(err, ErrNotJSON), errors.Is(err, ErrLineage), errors.Is(err, ErrSerialBehind):
		return fail(http.StatusConflict, err.Error())
	case err != nil:
		return err
	}
	if _, err := q.tx.Exec(q.ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
		VALUES ($1, 'user', $2, 'state.write', $3, nullif($4, '')::inet)`,
		q.orgID, q.userID, fmt.Sprintf("%s@%d", q.target, v.Serial), q.ip); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (h Handler) lock(w http.ResponseWriter, q request, body []byte) error {
	var want lockInfo
	if json.Unmarshal(body, &want) != nil || want.ID == "" {
		return fail(http.StatusBadRequest, "the lock request needs JSON lock info with an ID")
	}
	_, raw, isLocked, err := currentLock(q)
	if err != nil {
		return err
	}
	if isLocked {
		return locked(raw)
	}
	tag, err := q.tx.Exec(q.ctx, `INSERT INTO state_locks (workspace_id, org_id, lock_id, info, locked_by) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id) DO NOTHING`, q.workspaceID, q.orgID, want.ID, body, q.userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, raw, _, err := currentLock(q)
		if err != nil {
			return err
		}
		return locked(raw)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (h Handler) unlock(w http.ResponseWriter, q request, body []byte) error {
	var want lockInfo
	_ = json.Unmarshal(body, &want)
	l, raw, isLocked, err := currentLock(q)
	if err != nil {
		return err
	}
	if isLocked && l.ID != want.ID {
		return &httpError{status: http.StatusConflict, msg: "locked by another", body: raw}
	}
	if _, err := q.tx.Exec(q.ctx, `DELETE FROM state_locks WHERE workspace_id = $1`, q.workspaceID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

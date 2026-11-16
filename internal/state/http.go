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

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
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

// reply is written after the transaction commits, so a client never sees
// success for a write that rolled back.
type reply struct {
	status int
	body   []byte
	json   bool
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rep, err := h.serve(w, r)
	if err == nil {
		if rep.json {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(rep.body)
		}
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

func (h Handler) serve(w http.ResponseWriter, r *http.Request) (reply, error) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/state/"), "/")
	if len(parts) != 3 {
		return reply{}, errNotFound
	}
	org, project, workspace := parts[0], parts[1], parts[2]

	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, "LOCK", "UNLOCK":
	case http.MethodDelete:
		return reply{}, fail(http.StatusMethodNotAllowed, "Stackwright keeps state history; archive the workspace in Stackwright instead")
	default:
		return reply{}, fail(http.StatusMethodNotAllowed, "method not allowed")
	}

	_, token, ok := r.BasicAuth()
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="stackwright"`)
		return reply{}, fail(http.StatusUnauthorized, "set TF_HTTP_PASSWORD to a Stackwright API token")
	}
	var userID, jobID, tokenOrg, jobWorkspace string
	var err error
	// An executor from before state tokens sends its job token.
	if state := strings.HasPrefix(token, auth.StateTokenPrefix); state || strings.HasPrefix(token, auth.JobTokenPrefix) {
		column := "token_hash"
		if state {
			column = "state_token_hash"
		}
		err = h.Pool.QueryRow(r.Context(), `UPDATE job_leases SET heartbeat_at = now()
			WHERE `+column+` = $1 AND expires_at > now() RETURNING job_id, org_id, workspace_id`,
			auth.HashToken(token)).Scan(&jobID, &tokenOrg, &jobWorkspace)
		if errors.Is(err, pgx.ErrNoRows) {
			return reply{}, fail(http.StatusUnauthorized, "the job token is unknown, or its job has ended")
		}
	} else {
		userID, tokenOrg, err = auth.LookupToken(r.Context(), h.Pool, token)
		if errors.Is(err, auth.ErrBadToken) {
			return reply{}, fail(http.StatusUnauthorized, err.Error())
		}
	}
	if err != nil {
		return reply{}, err
	}

	var body []byte
	if write {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSize))
		if err != nil {
			return reply{}, fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("state is larger than %d MiB", MaxSize>>20))
		}
	}

	var rep reply
	err = pgx.BeginFunc(r.Context(), h.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `SELECT set_config('stackwright.user_id', $1, true), set_config('stackwright.org_id', $2, true)`,
			userID, tokenOrg); err != nil {
			return err
		}
		var orgID, operation string
		role := v1.Role_ROLE_UNSPECIFIED
		if jobID != "" {
			err = tx.QueryRow(r.Context(), `SELECT o.id, j.operation FROM orgs o JOIN jobs j ON j.org_id = o.id
				WHERE o.slug = $1 AND o.id = app_org_id() AND j.id = $2`, org, jobID).Scan(&orgID, &operation)
		} else {
			var dbRole string
			err = tx.QueryRow(r.Context(), `SELECT o.id, m.role FROM orgs o JOIN memberships m ON m.org_id = o.id
				WHERE o.slug = $1 AND m.user_id = app_user_id() AND o.id = app_org_id()`, org).Scan(&orgID, &dbRole)
			role = authz.RoleFromDB(dbRole)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case jobID == "" && write && !authz.AtLeast(role, v1.Role_ROLE_WRITER):
			return fail(http.StatusForbidden, "writing state needs the writer role or above")
		case jobID != "" && r.Method == http.MethodPost && operation != "apply":
			return fail(http.StatusForbidden, "a plan job cannot write state")
		}
		var workspaceID, backend string
		err = tx.QueryRow(r.Context(), `SELECT w.id, p.state_backend FROM workspaces w JOIN projects p ON p.id = w.project_id
			WHERE p.slug = $1 AND w.name = $2 AND p.archived_at IS NULL AND w.archived_at IS NULL`,
			project, workspace).Scan(&workspaceID, &backend)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && jobID != "" && workspaceID != jobWorkspace) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		// A second copy of the state would drift from the one the code's
		// backend holds (adr/0036).
		if backend != "stackwright" {
			return fail(http.StatusConflict, "this project's state lives in the backend its code names, not in Stackwright")
		}
		req := request{ctx: r.Context(), tx: tx, orgID: orgID, workspaceID: workspaceID, userID: userID, jobID: jobID,
			admin: authz.AtLeast(role, v1.Role_ROLE_ADMIN), target: "workspace:" + project + "/" + workspace, ip: auth.ClientIP(r)}

		switch r.Method {
		case http.MethodGet, http.MethodHead:
			rep, err = h.get(req)
		case http.MethodPost:
			rep, err = h.post(r, req, body)
		case "LOCK":
			rep, err = h.lock(req, body)
		default:
			rep, err = h.unlock(req, body)
		}
		return err
	})
	return rep, err
}

type request struct {
	ctx                                       context.Context
	tx                                        pgx.Tx
	orgID, workspaceID, userID, jobID, target string
	admin                                     bool
	ip                                        string
}

func (h Handler) get(q request) (reply, error) {
	v, ok, err := Latest(q.ctx, q.tx, q.workspaceID)
	if err != nil {
		return reply{}, err
	}
	if !ok {
		return reply{status: http.StatusNoContent}, nil
	}
	body, err := h.Store.Read(q.ctx, q.tx, q.orgID, v.BlobKey)
	if err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK, body: body, json: true}, nil
}

type lock struct {
	info      lockInfo
	raw       []byte
	user, job string
	held      bool
}

func currentLock(q request) (lock, error) {
	var l lock
	err := q.tx.QueryRow(q.ctx, `SELECT info, coalesce(locked_by::text, ''), coalesce(job_id::text, '')
		FROM state_locks WHERE workspace_id = $1 FOR UPDATE`, q.workspaceID).Scan(&l.raw, &l.user, &l.job)
	if errors.Is(err, pgx.ErrNoRows) {
		return lock{}, nil
	}
	if err != nil {
		return lock{}, err
	}
	_ = json.Unmarshal(l.raw, &l.info)
	l.held = true
	return l, nil
}

func locked(raw []byte) error {
	return &httpError{status: http.StatusLocked, msg: "locked", body: raw}
}

func (h Handler) post(r *http.Request, q request, body []byte) (reply, error) {
	l, err := currentLock(q)
	if err != nil {
		return reply{}, err
	}
	if l.held && r.URL.Query().Get("ID") != l.info.ID {
		return reply{}, locked(l.raw)
	}
	v, err := h.Store.Write(q.ctx, q.tx, q.orgID, q.workspaceID, q.userID, body)
	switch {
	case errors.Is(err, ErrNotJSON), errors.Is(err, ErrLineage), errors.Is(err, ErrSerialBehind), errors.Is(err, ErrSerialRange):
		return reply{}, fail(http.StatusConflict, err.Error())
	case err != nil:
		return reply{}, err
	}
	actorKind, actor := "user", q.userID
	if q.jobID != "" {
		actorKind, actor = "job", q.jobID
	}
	if _, err := q.tx.Exec(q.ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
		VALUES ($1, $2, $3, 'state.write', $4, nullif($5, '')::inet)`,
		q.orgID, actorKind, actor, fmt.Sprintf("%s@%d", q.target, v.Serial), q.ip); err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK}, nil
}

func (h Handler) lock(q request, body []byte) (reply, error) {
	var want lockInfo
	if json.Unmarshal(body, &want) != nil || want.ID == "" {
		return reply{}, fail(http.StatusBadRequest, "the lock request needs JSON lock info with an ID")
	}
	l, err := currentLock(q)
	if err != nil {
		return reply{}, err
	}
	if l.held {
		return reply{}, locked(l.raw)
	}
	tag, err := q.tx.Exec(q.ctx, `INSERT INTO state_locks (workspace_id, org_id, lock_id, info, locked_by, job_id)
		VALUES ($1, $2, $3, $4, nullif($5, '')::uuid, nullif($6, '')::uuid)
		ON CONFLICT (workspace_id) DO NOTHING`, q.workspaceID, q.orgID, want.ID, body, q.userID, q.jobID)
	if err != nil {
		return reply{}, err
	}
	if tag.RowsAffected() == 0 {
		l, err := currentLock(q)
		if err != nil {
			return reply{}, err
		}
		return reply{}, locked(l.raw)
	}
	return reply{status: http.StatusOK}, nil
}

// unlock releases the caller's own lock. Another holder's lock needs an
// admin, as ForceUnlockState does, and is audited the same way.
func (h Handler) unlock(q request, body []byte) (reply, error) {
	var want lockInfo
	_ = json.Unmarshal(body, &want)
	l, err := currentLock(q)
	if err != nil {
		return reply{}, err
	}
	if !l.held {
		return reply{status: http.StatusOK}, nil
	}
	if l.info.ID != want.ID {
		return reply{}, &httpError{status: http.StatusConflict, msg: "locked by another", body: l.raw}
	}
	own := (q.jobID != "" && l.job == q.jobID) || (q.userID != "" && l.user == q.userID)
	if !own {
		if !q.admin {
			return reply{}, fail(http.StatusForbidden, "another run or person holds this lock; an admin can force-unlock it")
		}
		if _, err := q.tx.Exec(q.ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
			VALUES ($1, 'user', $2, 'StateService/ForceUnlockState', $3, nullif($4, '')::inet)`,
			q.orgID, q.userID, q.target, q.ip); err != nil {
			return reply{}, err
		}
	}
	if _, err := q.tx.Exec(q.ctx, `DELETE FROM state_locks WHERE workspace_id = $1`, q.workspaceID); err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK}, nil
}

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stackwright/stackwright/fixtures"
	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/runs"
)

const (
	maxSource     = 32 << 20
	timelineBatch = 500
	watchFallback = time.Second
	listRunsLimit = 50
	maxSourceName = 200
)

type Runs struct {
	Pool         *pgxpool.Pool
	Orchestrator *runs.Orchestrator
}

const runColumns = `r.id, r.number, p.slug, w.name, r.status, r.plan_only, r.source, r.source_sha256,
	coalesce(r.has_changes, false), coalesce(r.adds, 0), coalesce(r.changes, 0), coalesce(r.destroys, 0), coalesce(r.error, ''),
	coalesce(nullif(cu.name, ''), cu.email, ''), coalesce(nullif(du.name, ''), du.email, ''),
	r.created_at, r.updated_at, r.finished_at, r.last_seq
	FROM runs r JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id
	LEFT JOIN users cu ON cu.id = r.created_by LEFT JOIN users du ON du.id = r.decided_by`

func scanRun(row pgx.CollectableRow) (*v1.Run, error) {
	r := &v1.Run{}
	var status string
	var created, updated time.Time
	var finished *time.Time
	err := row.Scan(&r.Id, &r.Number, &r.Project, &r.Workspace, &status, &r.PlanOnly, &r.Source, &r.SourceSha256,
		&r.HasChanges, &r.Adds, &r.Changes, &r.Destroys, &r.Error, &r.CreatedBy, &r.DecidedBy, &created, &updated, &finished, &r.LastSeq)
	r.Status = v1.RunStatus(v1.RunStatus_value["RUN_STATUS_"+strings.ToUpper(status)])
	r.CreatedAt, r.UpdatedAt = timestamppb.New(created), timestamppb.New(updated)
	if finished != nil {
		r.FinishedAt = timestamppb.New(*finished)
	}
	return r, err
}

func queryRuns(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]*v1.Run, error) {
	rows, err := tx.Query(ctx, `SELECT `+runColumns+` WHERE `+where, args...)
	if err != nil {
		return nil, authz.Internal(err)
	}
	out, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, authz.Internal(err)
	}
	return out, nil
}

func oneRun(ctx context.Context, tx pgx.Tx, runID string) (*v1.Run, error) {
	rs, err := queryRuns(ctx, tx, `r.id = $1`, runID)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, errNotFound
	}
	return rs[0], nil
}

func findRun(ctx context.Context, tx pgx.Tx, project, workspace string, number int32) (string, error) {
	ws, err := workspaceID(ctx, tx, project, workspace)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM runs WHERE workspace_id = $1 AND number = $2`, ws, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", authz.Internal(err)
	}
	return id, nil
}

func (h Runs) CreateRun(ctx context.Context, req *connect.Request[v1.CreateRunRequest]) (*connect.Response[v1.CreateRunResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	ws, err := workspaceID(ctx, s.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	source, name := m.Source, strings.TrimSpace(m.SourceName)
	switch {
	case len(source) == 0:
		var sample bool
		if err := s.Tx.QueryRow(ctx, `SELECT p.sample FROM projects p JOIN workspaces w ON w.project_id = p.id WHERE w.id = $1`, ws).Scan(&sample); err != nil {
			return nil, authz.Internal(err)
		}
		if !sample {
			return nil, invalid("upload a .tar.gz of the repository; only a sample project runs without one")
		}
		if source, err = fixtures.Sample(); err != nil {
			return nil, authz.Internal(err)
		}
		name = "sample"
	case len(source) > maxSource:
		return nil, invalid(fmt.Sprintf("the upload is larger than %d MiB", maxSource>>20))
	case !bytes.HasPrefix(source, []byte{0x1f, 0x8b}):
		return nil, invalid("the upload is not a .tar.gz")
	case name == "":
		name = "upload"
	}
	if len(name) > maxSourceName {
		name = name[:maxSourceName]
	}
	sum := sha256.Sum256(source)
	id, number, err := h.Orchestrator.Create(ctx, s, ws, m.PlanOnly, source, name, hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("run:%s/%s#%d", m.Project, m.Workspace, number))
	run, err := oneRun(ctx, s.Tx, id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CreateRunResponse{Run: run}), nil
}

func (Runs) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	ws, err := workspaceID(ctx, tx, req.Msg.Project, req.Msg.Workspace)
	if err != nil {
		return nil, err
	}
	rs, err := queryRuns(ctx, tx, `r.workspace_id = $1 ORDER BY r.number DESC LIMIT $2`, ws, listRunsLimit)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListRunsResponse{Runs: rs}), nil
}

func (Runs) GetRun(ctx context.Context, req *connect.Request[v1.GetRunRequest]) (*connect.Response[v1.GetRunResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	id, err := findRun(ctx, tx, req.Msg.Project, req.Msg.Workspace, req.Msg.Number)
	if err != nil {
		return nil, err
	}
	run, err := oneRun(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetRunResponse{Run: run}), nil
}

func final(s v1.RunStatus) bool {
	switch s {
	case v1.RunStatus_RUN_STATUS_PLANNED, v1.RunStatus_RUN_STATUS_APPLIED, v1.RunStatus_RUN_STATUS_FAILED,
		v1.RunStatus_RUN_STATUS_REJECTED, v1.RunStatus_RUN_STATUS_CANCELED:
		return true
	}
	return false
}

// timeline holds both orchestrator and executor events. They share one
// sequence per run.
const timeline = `SELECT e.seq, e.type, '', e.payload::text, e.at, coalesce(nullif(u.name, ''), u.email, '')
	FROM run_events e LEFT JOIN users u ON u.id = e.actor_id WHERE e.run_id = $1 AND e.seq > $2
	UNION ALL
	SELECT je.run_seq, je.type, je.resource_addr, je.payload::text, je.at, ''
	FROM job_events je JOIN jobs j ON j.id = je.job_id WHERE j.run_id = $1 AND je.run_seq > $2
	ORDER BY 1 LIMIT $3`

func (h Runs) WatchRun(ctx context.Context, req *connect.Request[v1.WatchRunRequest], stream *connect.ServerStream[v1.WatchRunResponse]) error {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	var runID string
	if err := s.Read(ctx, h.Pool, func(tx pgx.Tx) (err error) {
		runID, err = findRun(ctx, tx, m.Project, m.Workspace, m.Number)
		return err
	}); err != nil {
		return err
	}
	changed, stop, err := h.Orchestrator.Subscribe(runID)
	if err != nil {
		return authz.Internal(err)
	}
	defer stop()
	tick := time.NewTicker(watchFallback)
	defer tick.Stop()

	cursor := m.AfterSeq
	var sent time.Time
	for {
		var run *v1.Run
		var events []*v1.RunEvent
		err := s.Read(ctx, h.Pool, func(tx pgx.Tx) error {
			var err error
			if run, err = oneRun(ctx, tx, runID); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, timeline, runID, cursor, timelineBatch)
			if err != nil {
				return authz.Internal(err)
			}
			events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.RunEvent, error) {
				e := &v1.RunEvent{}
				var at time.Time
				err := r.Scan(&e.Seq, &e.Type, &e.ResourceAddr, &e.Payload, &at, &e.Actor)
				e.At = timestamppb.New(at)
				return e, err
			})
			if err != nil {
				return authz.Internal(err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		resp := &v1.WatchRunResponse{Events: events}
		if updated := run.UpdatedAt.AsTime(); !updated.Equal(sent) {
			resp.Run, sent = run, updated
		}
		if len(events) > 0 || resp.Run != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
		if len(events) > 0 {
			cursor = events[len(events)-1].Seq
			if len(events) == timelineBatch {
				continue
			}
		}
		if final(run.Status) && cursor >= run.LastSeq {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}

type decision func(*runs.Orchestrator, context.Context, *authz.Scope, string) error

func (h Runs) decide(ctx context.Context, project, workspace string, number int32, verb string, d decision) (*v1.Run, error) {
	s := authz.ScopeFrom(ctx)
	id, err := findRun(ctx, s.Tx, project, workspace, number)
	if err != nil {
		return nil, err
	}
	if err := d(h.Orchestrator, ctx, s, id); err != nil {
		if _, ok := errors.AsType[*connect.Error](err); ok {
			return nil, err
		}
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("run:%s/%s#%d %s", project, workspace, number, verb))
	return oneRun(ctx, s.Tx, id)
}

func (h Runs) ApproveRun(ctx context.Context, req *connect.Request[v1.ApproveRunRequest]) (*connect.Response[v1.ApproveRunResponse], error) {
	m := req.Msg
	run, err := h.decide(ctx, m.Project, m.Workspace, m.Number, "approved", (*runs.Orchestrator).Approve)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ApproveRunResponse{Run: run}), nil
}

func (h Runs) RejectRun(ctx context.Context, req *connect.Request[v1.RejectRunRequest]) (*connect.Response[v1.RejectRunResponse], error) {
	m := req.Msg
	run, err := h.decide(ctx, m.Project, m.Workspace, m.Number, "rejected", (*runs.Orchestrator).Reject)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.RejectRunResponse{Run: run}), nil
}

func (h Runs) CancelRun(ctx context.Context, req *connect.Request[v1.CancelRunRequest]) (*connect.Response[v1.CancelRunResponse], error) {
	m := req.Msg
	run, err := h.decide(ctx, m.Project, m.Workspace, m.Number, "canceled", (*runs.Orchestrator).Cancel)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CancelRunResponse{Run: run}), nil
}

func (Runs) ListWaiting(ctx context.Context, _ *connect.Request[v1.ListWaitingRequest]) (*connect.Response[v1.ListWaitingResponse], error) {
	s := authz.ScopeFrom(ctx)
	out := &v1.ListWaitingResponse{}
	var err error
	if authz.AtLeast(s.Role, v1.Role_ROLE_REVIEWER) {
		if out.Approvals, err = queryRuns(ctx, s.Tx, `r.status = 'needs_approval' AND w.archived_at IS NULL ORDER BY r.created_at`); err != nil {
			return nil, err
		}
	}
	if out.Failures, err = queryRuns(ctx, s.Tx, `r.status = 'failed' AND w.archived_at IS NULL
		AND r.id IN (SELECT DISTINCT ON (workspace_id) id FROM runs ORDER BY workspace_id, number DESC)
		ORDER BY r.finished_at DESC`); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

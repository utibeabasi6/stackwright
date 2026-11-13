// Package runs moves each run through its states (docs/BUILD.md, phase 5).
// Postgres holds the queue and the record (adr/0020); NATS only tells
// watchers that a run changed.
package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/nats-io/nats.go"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/seal"
)

var lockEntry = regexp.MustCompile(`provider "([a-z0-9.-]+/[a-z0-9_-]+/[a-z0-9_-]+)" \{\s+version\s+=\s+"([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*)"`)

// lockProviders lists a lock file's providers as "source version".
func lockProviders(lock []byte) []string {
	out := []string{}
	for _, m := range lockEntry.FindAllSubmatch(lock, 100) {
		out = append(out, string(m[1])+" "+string(m[2]))
	}
	return out
}

// Active is every status that holds its workspace.
var Active = []string{"planning", "needs_approval", "applying"}

type Orchestrator struct {
	Pool   *pgxpool.Pool
	KEK    seal.KEK
	S3     *minio.Client
	Bucket string
	Jobs   *jobs.Service
	Hub    *builders.Hub
	NATS   *nats.Conn

	once sync.Once
	wake chan struct{}
}

func subject(runID string) string { return "runs." + runID }

func sourceKey(orgID, runID string) string {
	return fmt.Sprintf("orgs/%s/runs/%s/source.tar.gz", orgID, runID)
}

func (o *Orchestrator) wakeChan() chan struct{} {
	o.once.Do(func() { o.wake = make(chan struct{}, 1) })
	return o.wake
}

// Wake asks the dispatcher to look at the queue now.
func (o *Orchestrator) Wake() {
	select {
	case o.wakeChan() <- struct{}{}:
	default:
	}
}

// Changed tells watchers of a run that it has news.
func (o *Orchestrator) Changed(runID string) {
	if err := o.NATS.Publish(subject(runID), nil); err != nil {
		slog.Warn("publish run change", "run", runID, "err", err)
	}
}

// Subscribe returns a channel that receives when the run changes.
func (o *Orchestrator) Subscribe(runID string) (<-chan struct{}, func(), error) {
	ch := make(chan struct{}, 1)
	sub, err := o.NATS.Subscribe(subject(runID), func(*nats.Msg) {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return ch, func() { _ = sub.Unsubscribe() }, nil
}

func (o *Orchestrator) afterCommit(ctx context.Context, runID string) {
	authz.AfterCommit(ctx, func() {
		o.Changed(runID)
		o.Wake()
	})
}

func appendEvent(ctx context.Context, tx pgx.Tx, runID, typ string, payload map[string]any, actorID string) error {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH r AS (UPDATE runs SET last_seq = last_seq + 1, updated_at = now() WHERE id = $1 RETURNING org_id, last_seq)
		INSERT INTO run_events (run_id, org_id, seq, type, payload, actor_id)
		SELECT $1, r.org_id, r.last_seq, $2, $3, nullif($4, '')::uuid FROM r`, runID, typ, b, actorID)
	return err
}

func finish(ctx context.Context, tx pgx.Tx, runID, status, msg, actorID string) error {
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = $2, error = nullif($3, ''), finished_at = now(), updated_at = now(),
		decided_by = coalesce(nullif($4, '')::uuid, decided_by) WHERE id = $1`, runID, status, msg, actorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_queue WHERE run_id = $1`, runID); err != nil {
		return err
	}
	payload := map[string]any{}
	if msg != "" {
		payload["error"] = msg
	}
	return appendEvent(ctx, tx, runID, "run."+status, payload, actorID)
}

func latestSerial(ctx context.Context, tx pgx.Tx, workspaceID string) (int64, error) {
	var serial int64
	err := tx.QueryRow(ctx, `SELECT coalesce(max(serial), -1) FROM state_versions WHERE workspace_id = $1`, workspaceID).Scan(&serial)
	return serial, err
}

// Create queues a run. s.Tx needs the org setting.
// NewRun is a run to queue. Its code is Source, which the run seals for
// itself, or SourceKey, a sealed archive that runs share.
type NewRun struct {
	WorkspaceID string
	PlanOnly    bool
	Source      []byte
	SourceKey   string
	SourceName  string
	SourceSHA   string
	CreatedBy   string
	// Trigger is "ui", "pull_request", "push" or "agent". Pull requests and
	// pushes need a commit; an agent run needs AgentSessionID and is
	// plan-only.
	Trigger        string
	AgentSessionID string
	CommitSHA      string
	Branch         string
	PRNumber       int
	RepoID         string
}

// Create queues a run. tx needs the org setting.
func (o *Orchestrator) Create(ctx context.Context, tx pgx.Tx, orgID string, r NewRun) (string, int32, error) {
	var number int32
	var gitState bool
	if err := tx.QueryRow(ctx, `UPDATE workspaces w SET runs_started = runs_started + 1 FROM projects p
		WHERE w.id = $1 AND p.id = w.project_id RETURNING w.runs_started, p.state_backend = 'git'`,
		r.WorkspaceID).Scan(&number, &gitState); err != nil {
		return "", 0, err
	}
	// State committed to Git is plan-only, whatever starts the run
	// (adr/0036).
	if gitState {
		r.PlanOnly = true
	}
	runID := uuid.NewString()
	if r.SourceKey == "" {
		sealed, err := o.KEK.Seal(ctx, tx, orgID, r.Source)
		if err != nil {
			return "", 0, err
		}
		if _, err := o.S3.PutObject(ctx, o.Bucket, sourceKey(orgID, runID), bytes.NewReader(sealed), int64(len(sealed)),
			minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
			return "", 0, fmt.Errorf("store run source: %w", err)
		}
	}
	trigger := r.Trigger
	if trigger == "" {
		trigger = "ui"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (id, org_id, workspace_id, number, plan_only, source, source_sha256, created_by,
		trigger, commit_sha, branch, pr_number, repo_id, source_key, agent_session_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::uuid, $9, nullif($10, ''), nullif($11, ''), nullif($12, 0), nullif($13, '')::uuid, nullif($14, ''),
			nullif($15, '')::uuid)`,
		runID, orgID, r.WorkspaceID, number, r.PlanOnly, r.SourceName, r.SourceSHA, r.CreatedBy,
		trigger, r.CommitSHA, r.Branch, r.PRNumber, r.RepoID, r.SourceKey, r.AgentSessionID); err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_queue (run_id, org_id) VALUES ($1, $2)`, runID, orgID); err != nil {
		return "", 0, err
	}
	if err := appendEvent(ctx, tx, runID, "run.queued", map[string]any{
		"source": r.SourceName, "plan_only": r.PlanOnly, "trigger": trigger, "commit": r.CommitSHA, "pr": r.PRNumber, "branch": r.Branch,
	}, r.CreatedBy); err != nil {
		return "", 0, err
	}
	o.afterCommit(ctx, runID)
	return runID, number, nil
}

func (o *Orchestrator) inOrg(ctx context.Context, orgID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, o.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Run starts queued runs and dispatches their jobs until ctx ends.
func (o *Orchestrator) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if err := o.dispatch(ctx); err != nil && ctx.Err() == nil {
			slog.Error("dispatch runs", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-o.wakeChan():
		}
	}
}

func (o *Orchestrator) dispatch(ctx context.Context) error {
	rows, err := o.Pool.Query(ctx, `SELECT run_id, org_id FROM run_queue ORDER BY queued_at`)
	if err != nil {
		return err
	}
	type queued struct{ run, org string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (queued, error) {
		var q queued
		return q, r.Scan(&q.run, &q.org)
	})
	if err != nil {
		return err
	}
	for _, q := range all {
		if err := o.step(ctx, q.run, q.org); err != nil && ctx.Err() == nil {
			slog.Error("advance run", "run", q.run, "err", err)
		}
	}
	return nil
}

type runRow struct {
	id, orgID, status, workspaceID string
	sourceKey                      *string
	planJobID, applyJobID          *string
	createdBy                      *string
	stateSerial                    *int64
}

func (o *Orchestrator) step(ctx context.Context, runID, orgID string) error {
	var r runRow
	var started bool
	err := o.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id, org_id, status, workspace_id, plan_job_id, apply_job_id, created_by, state_serial, source_key
			FROM runs WHERE id = $1 FOR UPDATE`, runID).
			Scan(&r.id, &r.orgID, &r.status, &r.workspaceID, &r.planJobID, &r.applyJobID, &r.createdBy, &r.stateSerial, &r.sourceKey)
		if err != nil || r.status != "queued" {
			return err
		}
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM runs WHERE workspace_id = $1 AND status = ANY($2))`,
			r.workspaceID, Active).Scan(&busy); err != nil || busy {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'planning', updated_at = now() WHERE id = $1`, runID); err != nil {
			return err
		}
		r.status, started = "planning", true
		return appendEvent(ctx, tx, runID, "run.planning", nil, "")
	})
	if err != nil {
		return err
	}
	if started {
		o.Changed(runID)
	}

	switch {
	case r.status == "planning" && r.planJobID == nil:
		return o.startJob(ctx, r, v1.JobOperation_JOB_OPERATION_PLAN)
	case r.status == "applying" && r.applyJobID == nil && r.planJobID != nil:
		return o.startJob(ctx, r, v1.JobOperation_JOB_OPERATION_APPLY)
	}
	return nil
}

func (o *Orchestrator) startJob(ctx context.Context, r runRow, op v1.JobOperation) error {
	var labels map[string]string
	var providers []string
	platform := ""
	if err := o.inOrg(ctx, r.orgID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT required_labels, providers FROM workspaces WHERE id = $1`, r.workspaceID).
			Scan(&labels, &providers); err != nil {
			return err
		}
		if op == v1.JobOperation_JOB_OPERATION_APPLY {
			return tx.QueryRow(ctx, `SELECT platform FROM jobs WHERE id = $1`, *r.planJobID).Scan(&platform)
		}
		return nil
	}); err != nil {
		return err
	}
	slot, ok := o.Hub.Reserve(r.orgID, labels, platform)
	if !ok {
		return o.inOrg(ctx, r.orgID, func(tx pgx.Tx) error {
			var last string
			if err := tx.QueryRow(ctx, `SELECT type FROM run_events WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`, r.id).Scan(&last); err != nil {
				return err
			}
			if last == "run.waiting" {
				return nil
			}
			defer o.afterCommit(ctx, r.id)
			return appendEvent(ctx, tx, r.id, "run.waiting", map[string]any{"labels": labels, "platform": platform}, "")
		})
	}
	req := jobs.Request{OrgID: r.orgID, WorkspaceID: r.workspaceID, Operation: op, Platform: slot.Platform, RunID: r.id,
		BuildServerID: slot.ID, Remote: slot.Remote}
	if r.createdBy != nil {
		req.CreatedBy = *r.createdBy
	}
	err := o.inOrg(ctx, r.orgID, func(tx pgx.Tx) error {
		if op == v1.JobOperation_JOB_OPERATION_APPLY {
			req.PlanJobID = *r.planJobID
			return o.refuseStale(ctx, tx, r)
		}
		key := sourceKey(r.orgID, r.id)
		if r.sourceKey != nil {
			key = *r.sourceKey
		}
		obj, err := o.S3.GetObject(ctx, o.Bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer func() { _ = obj.Close() }()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(obj); err != nil {
			return err
		}
		req.Source, err = o.KEK.Open(ctx, tx, r.orgID, buf.Bytes())
		return err
	})
	if err != nil {
		slot.Release()
		if errors.Is(err, errStale) {
			return nil
		}
		return err
	}

	created, err := o.Jobs.Create(ctx, req)
	jobID := created.ID
	if err != nil {
		slot.Release()
		return o.inOrg(ctx, r.orgID, func(tx pgx.Tx) error {
			defer o.Changed(r.id)
			return finish(ctx, tx, r.id, "failed", err.Error(), "")
		})
	}
	column, operation := "plan_job_id", "plan"
	if op == v1.JobOperation_JOB_OPERATION_APPLY {
		column, operation = "apply_job_id", "apply"
	}
	err = o.inOrg(ctx, r.orgID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runs SET `+column+` = $2, updated_at = now() WHERE id = $1`, r.id, jobID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, r.id, "run.dispatched", map[string]any{"job_id": jobID, "operation": operation, "build_server": slot.Builder}, "")
	})
	if err != nil {
		slot.Release()
		return errors.Join(err, o.Jobs.Abandon(ctx, jobID, "the job could not be recorded on its run"))
	}
	if err := slot.Start(&v1.StartSandbox{JobId: jobID, Ticket: created.Ticket, Engine: created.Engine, Providers: providers}); err != nil {
		return o.Jobs.Abandon(ctx, jobID, "the build server went away before the sandbox started")
	}
	o.Changed(r.id)
	return nil
}

var errStale = errors.New("plan is stale")

// refuseStale fails a run whose plan no longer matches the state.
func (o *Orchestrator) refuseStale(ctx context.Context, tx pgx.Tx, r runRow) error {
	serial, err := latestSerial(ctx, tx, r.workspaceID)
	if err != nil {
		return err
	}
	if r.stateSerial == nil || serial == *r.stateSerial {
		return nil
	}
	msg := fmt.Sprintf("plan is stale: the state changed after the plan (serial %d, now %d). Start a new run.", *r.stateSerial, serial)
	if err := finish(ctx, tx, r.id, "failed", msg, ""); err != nil {
		return err
	}
	o.afterCommit(ctx, r.id)
	return errStale
}

// JobEnded moves a run on when one of its jobs ends. It runs in the
// transaction that ended the job.
func (o *Orchestrator) JobEnded(ctx context.Context, tx pgx.Tx, jobID, status, msg string) error {
	var runID *string
	var operation string
	var hasChanges *bool
	if err := tx.QueryRow(ctx, `SELECT run_id, operation, has_changes FROM jobs WHERE id = $1`, jobID).Scan(&runID, &operation, &hasChanges); err != nil {
		return err
	}
	if runID == nil {
		return nil
	}
	var runStatus, workspaceID string
	var planOnly, autoApply bool
	if err := tx.QueryRow(ctx, `SELECT r.status, r.workspace_id, r.plan_only, w.auto_apply FROM runs r
		JOIN workspaces w ON w.id = r.workspace_id WHERE r.id = $1 FOR UPDATE OF r`, *runID).
		Scan(&runStatus, &workspaceID, &planOnly, &autoApply); err != nil {
		return err
	}
	defer o.afterCommit(ctx, *runID)
	failure := msg
	if failure == "" {
		failure = "the " + operation + " failed"
	}

	switch {
	case operation == "apply" && runStatus == "applying":
		if status != "succeeded" {
			return finish(ctx, tx, *runID, "failed", failure, "")
		}
		return finish(ctx, tx, *runID, "applied", "", "")
	case operation == "plan" && runStatus == "planning":
		if status != "succeeded" {
			return finish(ctx, tx, *runID, "failed", failure, "")
		}
	default:
		return nil
	}

	lock, err := o.Jobs.ReadArtifact(ctx, tx, jobID, "lock.hcl")
	switch {
	case err == nil || errors.Is(err, jobs.ErrNoArtifact):
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET providers = $2 WHERE id = $1`, workspaceID, lockProviders(lock)); err != nil {
			return err
		}
	default:
		slog.Warn("read the plan's lock file", "job", jobID, "err", err)
	}

	var adds, changes, destroys int
	rows, err := tx.Query(ctx, `SELECT payload->'change'->>'action', count(*) FROM job_events
		WHERE job_id = $1 AND type = 'engine.planned_change' GROUP BY 1`, jobID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			rows.Close()
			return err
		}
		switch action {
		case "create":
			adds += n
		case "update":
			changes += n
		case "delete":
			destroys += n
		case "replace":
			adds += n
			destroys += n
		}
	}
	rows.Close()
	serial, err := latestSerial(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	changed := hasChanges != nil && *hasChanges
	if _, err := tx.Exec(ctx, `UPDATE runs SET has_changes = $2, adds = $3, changes = $4, destroys = $5, state_serial = $6
		WHERE id = $1`, *runID, changed, adds, changes, destroys, serial); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, *runID, "run.summary", map[string]any{
		"has_changes": changed, "adds": adds, "changes": changes, "destroys": destroys,
	}, ""); err != nil {
		return err
	}
	next, event := "needs_approval", "run.needs_approval"
	switch {
	case !changed || planOnly:
		return finish(ctx, tx, *runID, "planned", "", "")
	case autoApply:
		next, event = "applying", "run.auto_approved"
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = $2, updated_at = now() WHERE id = $1`, *runID, next); err != nil {
		return err
	}
	return appendEvent(ctx, tx, *runID, event, nil, "")
}

// SandboxExited fails a job whose sandbox stopped before the job ended.
func (o *Orchestrator) SandboxExited(jobID string, st *v1.SandboxStatus) {
	msg := fmt.Sprintf("the sandbox exited with code %d before the job finished", st.GetExitCode())
	if st.GetState() == v1.SandboxStatus_STATE_FAILED {
		msg = "the sandbox did not start: " + st.GetError()
	}
	if err := o.Jobs.Abandon(context.Background(), jobID, msg); err != nil {
		slog.Error("fail an abandoned job", "job", jobID, "err", err)
	}
}

func refuse(code connect.Code, format string, args ...any) error {
	return connect.NewError(code, fmt.Errorf(format, args...))
}

// Approve starts the apply, unless the state changed after the plan; then
// the run fails, and Approve returns no error. s.Tx needs the org setting.
func (o *Orchestrator) Approve(ctx context.Context, s *authz.Scope, runID string) error {
	var r runRow
	if err := s.Tx.QueryRow(ctx, `SELECT id, org_id, status, workspace_id, state_serial FROM runs WHERE id = $1 FOR UPDATE`, runID).
		Scan(&r.id, &r.orgID, &r.status, &r.workspaceID, &r.stateSerial); err != nil {
		return err
	}
	if r.status != "needs_approval" {
		return refuse(connect.CodeFailedPrecondition, "the run is %s, so it waits for no approval", r.status)
	}
	if err := o.refuseStale(ctx, s.Tx, r); errors.Is(err, errStale) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE runs SET status = 'applying', decided_by = $2, updated_at = now() WHERE id = $1`, runID, s.UserID); err != nil {
		return err
	}
	o.afterCommit(ctx, runID)
	return appendEvent(ctx, s.Tx, runID, "run.approved", nil, s.UserID)
}

func (o *Orchestrator) Reject(ctx context.Context, s *authz.Scope, runID string) error {
	var status string
	if err := s.Tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status); err != nil {
		return err
	}
	if status != "needs_approval" {
		return refuse(connect.CodeFailedPrecondition, "the run is %s, so it waits for no approval", status)
	}
	o.afterCommit(ctx, runID)
	return finish(ctx, s.Tx, runID, "rejected", "", s.UserID)
}

// Cancel ends a run that is not finished. Its sandbox, if any, gets a
// graceful stop, so the engine can release its state lock.
func (o *Orchestrator) Cancel(ctx context.Context, s *authz.Scope, runID string) error {
	var status string
	var planJob, applyJob *string
	if err := s.Tx.QueryRow(ctx, `SELECT status, plan_job_id, apply_job_id FROM runs WHERE id = $1 FOR UPDATE`, runID).
		Scan(&status, &planJob, &applyJob); err != nil {
		return err
	}
	var job *string
	switch status {
	case "queued", "needs_approval":
	case "planning":
		job = planJob
	case "applying":
		job = applyJob
	default:
		return refuse(connect.CodeFailedPrecondition, "the run is %s already", status)
	}
	if job != nil {
		authz.AfterCommit(ctx, func() { o.Hub.Stop(*job) })
	}
	o.afterCommit(ctx, runID)
	return finish(ctx, s.Tx, runID, "canceled", "", s.UserID)
}

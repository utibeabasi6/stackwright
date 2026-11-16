package runs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/db"
)

// DriftEvery is the interval of drift checks, from 00:00 UTC (adr/0042).
const DriftEvery = 6 * time.Hour

// ErrNeverApplied is a workspace with no applied run, so no code to check
// drift against or to revert to.
var ErrNeverApplied = errors.New("the workspace has never applied")

// LastApplied returns a run of the code that the workspace last applied.
// tx needs the org setting.
func (o *Orchestrator) LastApplied(ctx context.Context, tx pgx.Tx, orgID, workspaceID string) (NewRun, error) {
	r := NewRun{WorkspaceID: workspaceID}
	var runID string
	var key, commit, branch, repo *string
	err := tx.QueryRow(ctx, `SELECT id, source_key, source, source_sha256, commit_sha, branch, repo_id::text FROM runs
		WHERE workspace_id = $1 AND status = 'applied' ORDER BY number DESC LIMIT 1`, workspaceID).
		Scan(&runID, &key, &r.SourceName, &r.SourceSHA, &commit, &branch, &repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNeverApplied
	}
	if err != nil {
		return r, err
	}
	r.SourceKey = sourceKey(orgID, runID)
	if key != nil {
		r.SourceKey = *key
	}
	for dst, src := range map[*string]*string{&r.CommitSHA: commit, &r.Branch: branch, &r.RepoID: repo} {
		if src != nil {
			*dst = *src
		}
	}
	r.Inputs, err = o.lastInputs(ctx, tx, orgID, workspaceID)
	return r, err
}

// lastInputs returns the variables that the workspace's last applied run
// got from a deployment's links, so a later run of that code gets them too.
func (o *Orchestrator) lastInputs(ctx context.Context, tx pgx.Tx, orgID, workspaceID string) ([]byte, error) {
	var sealed []byte
	err := tx.QueryRow(ctx, `SELECT inputs FROM runs WHERE workspace_id = $1 AND status = 'applied' ORDER BY number DESC LIMIT 1`,
		workspaceID).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sealed == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o.KEK.Open(ctx, tx, orgID, sealed)
}

// Drift queues QueueDrift at each DriftEvery slot until ctx ends. A slot
// that passes while no server runs is skipped.
func (o *Orchestrator) Drift(ctx context.Context) {
	for {
		slot := time.Now().UTC().Truncate(DriftEvery).Add(DriftEvery)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(slot)):
		}
		if err := o.QueueDrift(ctx, slot); err != nil && ctx.Err() == nil {
			slog.Error("queue drift checks", "err", err)
		}
	}
}

// QueueDrift queues a refresh-only plan of the code last applied for each
// workspace that applied at least once and has no drift run since slot.
func (o *Orchestrator) QueueDrift(ctx context.Context, slot time.Time) error {
	rows, err := o.Pool.Query(ctx, `SELECT workspace_id, org_id FROM drift_checks_due($1)`, slot)
	if err != nil {
		return err
	}
	type due struct{ workspace, org string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.workspace, &d.org)
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range all {
		err := db.InOrg(ctx, o.Pool, d.org, func(tx pgx.Tx) error {
			r, err := o.LastApplied(ctx, tx, d.org, d.workspace)
			if err != nil {
				return err
			}
			r.PlanOnly, r.RefreshOnly, r.Trigger = true, true, "drift"
			_, _, err = o.Create(ctx, tx, d.org, r)
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(all) > 0 {
		o.Wake()
	}
	return errors.Join(errs...)
}

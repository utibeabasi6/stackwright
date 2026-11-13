package runs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// NightlyHour is the hour, in UTC, of the nightly plan-only review.
const NightlyHour = 2

// HeadFunc returns a run of the newest commit for a workspace whose project
// has a linked repository, and false when none covers it.
type HeadFunc func(ctx context.Context, tx pgx.Tx, orgID, workspaceID string) (NewRun, bool, error)

// Nightly queues QueueNightly at each NightlyHour until ctx ends. A night
// that passes while no server runs is skipped, not made up.
func (o *Orchestrator) Nightly(ctx context.Context, head HeadFunc) {
	for {
		now := time.Now().UTC()
		slot := time.Date(now.Year(), now.Month(), now.Day(), NightlyHour, 0, 0, 0, time.UTC)
		if !slot.After(now) {
			slot = slot.Add(24 * time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(slot)):
		}
		if err := o.QueueNightly(ctx, slot, head); err != nil && ctx.Err() == nil {
			slog.Error("queue nightly reviews", "err", err)
		}
	}
}

// QueueNightly queues a plan-only run with trigger "schedule" for each
// workspace that has none since slot, so findings follow new checks and
// expired acceptances when the code does not change (adr/0037). A linked
// workspace plans its branch's newest commit; any other plans the code of
// its last own run again.
func (o *Orchestrator) QueueNightly(ctx context.Context, slot time.Time, head HeadFunc) error {
	rows, err := o.Pool.Query(ctx, `SELECT workspace_id, org_id FROM nightly_reviews_due($1)`, slot)
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
		err := o.inOrg(ctx, d.org, func(tx pgx.Tx) error {
			r, linked, err := head(ctx, tx, d.org, d.workspace)
			if err != nil {
				return err
			}
			if !linked {
				var runID string
				var key *string
				err := tx.QueryRow(ctx, `SELECT id, source_key, source, source_sha256 FROM runs WHERE workspace_id = $1
					AND trigger IN ('ui', 'push', 'schedule') ORDER BY number DESC LIMIT 1`, d.workspace).
					Scan(&runID, &key, &r.SourceName, &r.SourceSHA)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				r.WorkspaceID, r.SourceKey = d.workspace, sourceKey(d.org, runID)
				if key != nil {
					r.SourceKey = *key
				}
			}
			r.PlanOnly, r.Trigger = true, "schedule"
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

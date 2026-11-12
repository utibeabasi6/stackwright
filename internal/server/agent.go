package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/internal/github"
	"github.com/stackwright/stackwright/internal/graph"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/runs"
)

const (
	maxDiagnostics = 20
	maxListed      = 100
	maxStateLines  = 400
)

// agentHooks are the agent's only way to jobs, runs and GitHub (adr/0031).
type agentHooks struct {
	pool   *pgxpool.Pool
	jobs   *jobs.Service
	runs   *runs.Orchestrator
	github *github.GitHub
}

func (h agentHooks) inOrg(ctx context.Context, orgID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (h agentHooks) plan(ctx context.Context, orgID, session, userID string, source []byte) (string, error) {
	var runID string
	var number int32
	err := h.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		var workspaceID, branch string
		if err := tx.QueryRow(ctx, `SELECT workspace_id, branch FROM agent_sessions WHERE id = $1`, session).Scan(&workspaceID, &branch); err != nil {
			return err
		}
		sum := sha256.Sum256(source)
		var err error
		runID, number, err = h.runs.Create(ctx, tx, orgID, runs.NewRun{
			WorkspaceID: workspaceID, PlanOnly: true, Source: source, SourceName: "agent · " + branch, SourceSHA: hex.EncodeToString(sum[:]),
			CreatedBy: userID, Trigger: "agent", AgentSessionID: session, Branch: branch,
		})
		return err
	})
	if err != nil {
		return "", err
	}
	h.runs.Wake()
	changed, stop, err := h.runs.Subscribe(runID)
	if err != nil {
		return "", err
	}
	defer stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		report, done, err := h.planReport(ctx, orgID, runID, number)
		if err != nil || done {
			return report, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-changed:
		case <-tick.C:
		}
	}
}

// planReport describes a finished run for the model: its summary and
// changes, or its error and the engine's error diagnostics, which the
// executor has already redacted.
func (h agentHooks) planReport(ctx context.Context, orgID, runID string, number int32) (string, bool, error) {
	var b strings.Builder
	done := false
	err := h.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		var status, msg string
		var jobID *string
		var adds, changes, destroys *int
		if err := tx.QueryRow(ctx, `SELECT status, coalesce(error, ''), plan_job_id, adds, changes, destroys FROM runs WHERE id = $1`, runID).
			Scan(&status, &msg, &jobID, &adds, &changes, &destroys); err != nil {
			return err
		}
		if status == "queued" || status == "planning" {
			return nil
		}
		done = true
		if status == "planned" {
			fmt.Fprintf(&b, "Run #%d planned: %d to add, %d to change, %d to destroy.\n", number, deref(adds), deref(changes), deref(destroys))
			rows, err := tx.Query(ctx, `SELECT payload->'change'->>'action', payload->'change'->'resource'->>'addr' FROM job_events
				WHERE job_id = $1 AND type = 'engine.planned_change' ORDER BY seq LIMIT $2`, *jobID, maxListed)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var action, addr string
				if err := rows.Scan(&action, &addr); err != nil {
					return err
				}
				fmt.Fprintf(&b, "  %s %s\n", action, addr)
			}
			return rows.Err()
		}
		fmt.Fprintf(&b, "Run #%d %s: %s\n", number, status, msg)
		if jobID == nil {
			return nil
		}
		// https://opentofu.org/docs/internals/machine-readable-ui/#diagnostic
		rows, err := tx.Query(ctx, `SELECT coalesce(payload->'diagnostic'->'range'->>'filename', ''),
			coalesce((payload->'diagnostic'->'range'->'start'->>'line')::int, 0), payload->'diagnostic'->>'summary',
			coalesce(payload->'diagnostic'->>'detail', '') FROM job_events
			WHERE job_id = $1 AND type = 'engine.diagnostic' AND payload->'diagnostic'->>'severity' = 'error' ORDER BY seq LIMIT $2`, *jobID, maxDiagnostics)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var file, summary, detail string
			var line int
			if err := rows.Scan(&file, &line, &summary, &detail); err != nil {
				return err
			}
			if file != "" {
				fmt.Fprintf(&b, "%s:%d: ", file, line)
			}
			fmt.Fprintf(&b, "%s\n", strings.TrimSpace(summary+": "+detail))
		}
		return rows.Err()
	})
	return b.String(), done, err
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// newestPlan finds the plan job of a run in the session's workspace whose
// plan succeeded: run number, or with run 0, the newest of match.
func newestPlan(ctx context.Context, tx pgx.Tx, session string, run int32, ownOnly bool) (int32, string, error) {
	var number int32
	var jobID string
	err := tx.QueryRow(ctx, `SELECT r.number, r.plan_job_id FROM runs r JOIN jobs j ON j.id = r.plan_job_id
		WHERE r.workspace_id = (SELECT workspace_id FROM agent_sessions WHERE id = $1) AND j.status = 'succeeded'
		AND ($2 = 0 OR r.number = $2) AND ($2 <> 0 OR NOT $3 OR r.agent_session_id = $1)
		ORDER BY r.number DESC LIMIT 1`, session, run, ownOnly).Scan(&number, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if run != 0 {
			return 0, "", fmt.Errorf("run #%d has no plan in this workspace", run)
		}
		return 0, "", errors.New("there is no plan to read yet; call run_plan first")
	}
	return number, jobID, err
}

func (h agentHooks) planDetails(ctx context.Context, orgID, session string, run int32, address string) (string, error) {
	var b strings.Builder
	err := h.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		number, jobID, err := newestPlan(ctx, tx, session, run, true)
		if err != nil {
			return err
		}
		plan, err := h.jobs.ReadArtifact(ctx, tx, jobID, "plan.json")
		if err != nil {
			return err
		}
		sources, err := h.jobs.ReadArtifact(ctx, tx, jobID, "sources.json")
		if err != nil && !errors.Is(err, jobs.ErrNoArtifact) {
			return err
		}
		id := graph.StripKeys(address)
		node, err := graph.Node(plan, sources, id)
		if errors.Is(err, graph.ErrNoNode) {
			return fmt.Errorf("run #%d has no resource %s", number, address)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "Run #%d:\n", number)
		for _, in := range node.Instances {
			if address != id && in.Address != address {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", in.Address, strings.ToLower(strings.TrimPrefix(in.Action.String(), "PLAN_ACTION_")))
			for _, c := range in.Changes {
				fmt.Fprintf(&b, "  %s: %s -> %s", c.Path, orNone(c.Before), orNone(c.After))
				if c.ForcesReplacement {
					b.WriteString(" (forces replacement)")
				}
				b.WriteByte('\n')
			}
			if in.Truncated {
				b.WriteString("  [more changes left out]\n")
			}
		}
		return nil
	})
	return b.String(), err
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

func (h agentHooks) stateLookup(ctx context.Context, orgID, session, address string) (string, error) {
	var out string
	err := h.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		number, jobID, err := newestPlan(ctx, tx, session, 0, false)
		if err != nil {
			return err
		}
		plan, err := h.jobs.ReadArtifact(ctx, tx, jobID, "plan.json")
		if err != nil {
			return err
		}
		lines, err := graph.StateResource(plan, address)
		if errors.Is(err, graph.ErrNoNode) {
			return fmt.Errorf("the state that run #%d read has no %s; give an instance address, such as aws_instance.web[0]", number, address)
		}
		if err != nil {
			return err
		}
		if len(lines) > maxStateLines {
			lines = append(lines[:maxStateLines], "[more attributes left out]")
		}
		out = fmt.Sprintf("%s in the state that run #%d read:\n%s\n", address, number, strings.Join(lines, "\n"))
		return nil
	})
	return out, err
}

func (h agentHooks) openPullRequest(ctx context.Context, orgID, session, title, body string) (int, string, error) {
	var repo, branch, base string
	var installation int64
	err := h.inOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rp.full_name, rp.installation_id, s.branch, s.base FROM agent_sessions s
			JOIN repos rp ON rp.id = s.repo_id WHERE s.id = $1`, session).Scan(&repo, &installation, &branch, &base)
	})
	if err != nil {
		return 0, "", err
	}
	return h.github.OpenPullRequest(ctx, installation, repo, branch, base, title, body)
}

package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/utibeabasi6/stackwright/internal/db"
)

const maxAnnotations = 50

// forkCheck tells a pull request from a fork that Stackwright does not
// plan it: its code would run with the org's variables and credentials.
func (g *GitHub) forkCheck(ctx context.Context, repo repoRow, sha string) error {
	token, err := g.installationToken(ctx, repo.installation)
	if err != nil {
		return err
	}
	return g.call(ctx, http.MethodPost, "/repos/"+repo.fullName+"/check-runs", token, map[string]any{
		"name": "stackwright", "head_sha": sha, "status": "completed", "conclusion": "neutral",
		"output": map[string]string{
			"title":   "Not planned: the pull request is from a fork",
			"summary": "Stackwright does not plan pull requests from forks, because their code would run with this org's variables and cloud credentials.",
		},
	}, nil)
}

type checkRun struct {
	runID, status                       string
	checkRunID                          *int64
	number                              int
	commit                              string
	pr                                  *int
	repoID, repo                        string
	installation                        int64
	hasChanges                          bool
	adds, changes, destroys             int
	errText                             string
	jobID                               *string
	orgSlug, project, folder, workspace string
}

// RunChecks keeps each run's check and its pull request's comment in step
// with the run, until ctx ends.
func (g *GitHub) RunChecks(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := g.syncChecks(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sync GitHub checks", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (g *GitHub) syncChecks(ctx context.Context) error {
	rows, err := g.Pool.Query(ctx, `SELECT DISTINCT org_id FROM github_checks WHERE next_attempt_at <= now()`)
	if err != nil {
		return err
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, org := range orgs {
		if err := g.syncOrg(ctx, org); err != nil && ctx.Err() == nil {
			slog.Warn("sync GitHub checks", "org", org, "err", err)
		}
	}
	return nil
}

func (g *GitHub) syncOrg(ctx context.Context, orgID string) error {
	return db.InOrg(ctx, g.Pool, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.run_id, r.status, c.check_run_id, r.number, r.commit_sha, r.pr_number,
			rp.id, rp.full_name, rp.installation_id, coalesce(r.has_changes, false), coalesce(r.adds, 0), coalesce(r.changes, 0),
			coalesce(r.destroys, 0), coalesce(r.error, ''), coalesce(r.apply_job_id, r.plan_job_id), o.slug, p.slug, p.path, w.name
			FROM github_checks c JOIN runs r ON r.id = c.run_id JOIN repos rp ON rp.id = r.repo_id
			JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id JOIN orgs o ON o.id = r.org_id
			WHERE c.org_id = $1 AND c.next_attempt_at <= now() AND c.synced_status <> r.status ORDER BY r.created_at`, orgID)
		if err != nil {
			return err
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (checkRun, error) {
			var c checkRun
			return c, r.Scan(&c.runID, &c.status, &c.checkRunID, &c.number, &c.commit, &c.pr, &c.repoID, &c.repo, &c.installation,
				&c.hasChanges, &c.adds, &c.changes, &c.destroys, &c.errText, &c.jobID, &c.orgSlug, &c.project, &c.folder, &c.workspace)
		})
		if err != nil {
			return err
		}
		pulls := map[[2]string]checkRun{}
		for _, c := range due {
			if err := g.syncCheck(ctx, tx, c); err != nil {
				slog.Warn("sync a GitHub check", "run", c.runID, "err", err)
				// After five tries for one status, it waits for the next.
				if _, err := tx.Exec(ctx, `UPDATE github_checks SET
					next_attempt_at = now() + (attempts + 1) * interval '30 seconds',
					synced_status = CASE WHEN attempts + 1 >= $2 THEN $3 ELSE synced_status END,
					attempts = CASE WHEN attempts + 1 >= $2 THEN 0 ELSE attempts + 1 END WHERE run_id = $1`, c.runID, maxAttempts, c.status); err != nil {
					return err
				}
				continue
			}
			if c.pr != nil {
				pulls[[2]string{c.repoID, fmt.Sprint(*c.pr)}] = c
			}
		}
		for _, c := range pulls {
			if err := g.syncComment(ctx, tx, orgID, c); err != nil {
				slog.Warn("update the pull request comment", "repo", c.repo, "pr", *c.pr, "err", err)
			}
		}
		return nil
	})
}

func (c checkRun) url(public string) string {
	return fmt.Sprintf("%s/%s/projects/%s/%s/runs/%d", public, c.orgSlug, c.project, c.workspace, c.number)
}

func (c checkRun) changesText() string {
	if !c.hasChanges {
		return "no changes"
	}
	return fmt.Sprintf("+%d ~%d −%d", c.adds, c.changes, c.destroys)
}

// state is the run's status as GitHub shows it, and as the comment says it.
// https://docs.github.com/en/rest/checks/runs#create-a-check-run
func (c checkRun) state() (status, conclusion, title string) {
	switch c.status {
	case "queued":
		return "queued", "", "Queued"
	case "planning":
		return "in_progress", "", "Planning"
	case "applying":
		return "in_progress", "", "Applying"
	case "needs_approval":
		return "completed", "action_required", "Waits for approval: " + c.changesText()
	case "blocked":
		return "completed", "action_required", "Blocked by a security finding: " + c.changesText()
	case "planned":
		return "completed", "success", "Planned: " + c.changesText()
	case "applied":
		return "completed", "success", "Applied: " + c.changesText()
	case "failed":
		return "completed", "failure", "Failed"
	case "rejected":
		return "completed", "neutral", "Rejected"
	}
	return "completed", "cancelled", "Canceled"
}

func (g *GitHub) syncCheck(ctx context.Context, tx pgx.Tx, c checkRun) error {
	token, err := g.installationToken(ctx, c.installation)
	if err != nil {
		return err
	}
	status, conclusion, title := c.state()
	summary := fmt.Sprintf("[Open run #%d](%s)", c.number, c.url(g.PublicURL))
	if c.errText != "" {
		summary += "\n\n" + c.errText
	}
	output := map[string]any{"title": title, "summary": summary}
	if c.status == "failed" && c.jobID != nil {
		notes, err := annotations(ctx, tx, *c.jobID, c.folder)
		if err != nil {
			return err
		}
		output["annotations"] = notes
	}
	body := map[string]any{
		"name": fmt.Sprintf("stackwright: %s/%s", c.project, c.workspace), "head_sha": c.commit, "status": status,
		"details_url": c.url(g.PublicURL), "external_id": c.runID, "output": output,
	}
	if conclusion != "" {
		body["conclusion"] = conclusion
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if c.checkRunID == nil {
		err = g.call(ctx, http.MethodPost, "/repos/"+c.repo+"/check-runs", token, body, &out)
	} else {
		out.ID = *c.checkRunID
		err = g.call(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", c.repo, *c.checkRunID), token, body, nil)
	}
	if err != nil {
		return err
	}
	// A run that ended needs its check no more, and the loop reads only the
	// checks that remain.
	if slices.Contains([]string{"applied", "planned", "failed", "rejected", "canceled"}, c.status) {
		_, err = tx.Exec(ctx, `DELETE FROM github_checks WHERE run_id = $1`, c.runID)
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE github_checks SET check_run_id = $2, synced_status = $3, attempts = 0, next_attempt_at = now() WHERE run_id = $1`,
		c.runID, out.ID, c.status)
	return err
}

// annotations points at each error that the engine reported, by its file
// in the repository and its lines.
// https://opentofu.org/docs/internals/machine-readable-ui/#diagnostic
func annotations(ctx context.Context, tx pgx.Tx, jobID, folder string) ([]map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT payload->'diagnostic'->'range'->>'filename', (payload->'diagnostic'->'range'->'start'->>'line')::int,
		(payload->'diagnostic'->'range'->'end'->>'line')::int, payload->'diagnostic'->>'summary', coalesce(payload->'diagnostic'->>'detail', '')
		FROM job_events WHERE job_id = $1 AND type = 'engine.diagnostic' AND payload->'diagnostic'->>'severity' = 'error'
		AND payload->'diagnostic'->'range'->>'filename' IS NOT NULL ORDER BY seq LIMIT $2`, jobID, maxAnnotations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []map[string]any{}
	for rows.Next() {
		var file, summary, detail string
		var start, end int
		if err := rows.Scan(&file, &start, &end, &summary, &detail); err != nil {
			return nil, err
		}
		// GitHub refuses an annotation whose title passes 255 characters or
		// whose message passes 64 KB.
		// https://docs.github.com/en/rest/checks/runs#update-a-check-run
		clip := func(s string, n int) string {
			if r := []rune(s); len(r) > n {
				return string(r[:n-1]) + "…"
			}
			return s
		}
		notes = append(notes, map[string]any{
			"path": path.Join(folder, file), "start_line": start, "end_line": max(start, end), "annotation_level": "failure",
			"title": clip(summary, 255), "message": clip(strings.TrimSpace(summary+"\n"+detail), 16000),
		})
	}
	return notes, rows.Err()
}

// syncComment writes one comment for the pull request, with the runs of
// its newest commit, and edits it in place after that.
func (g *GitHub) syncComment(ctx context.Context, tx pgx.Tx, orgID string, c checkRun) error {
	rows, err := tx.Query(ctx, `SELECT r.status, r.number, coalesce(r.has_changes, false), coalesce(r.adds, 0), coalesce(r.changes, 0),
		coalesce(r.destroys, 0), p.slug, w.name, r.commit_sha
		FROM runs r JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id
		WHERE r.repo_id = $1 AND r.pr_number = $2 AND r.commit_sha = (
			SELECT commit_sha FROM runs WHERE repo_id = $1 AND pr_number = $2 ORDER BY created_at DESC LIMIT 1)
		ORDER BY p.slug, w.name`, c.repoID, *c.pr)
	if err != nil {
		return err
	}
	var table strings.Builder
	commit := c.commit
	for rows.Next() {
		r := c
		if err := rows.Scan(&r.status, &r.number, &r.hasChanges, &r.adds, &r.changes, &r.destroys, &r.project, &r.workspace, &commit); err != nil {
			rows.Close()
			return err
		}
		_, _, title := r.state()
		fmt.Fprintf(&table, "| [%s/%s](%s) | %s |\n", r.project, r.workspace, r.url(g.PublicURL), title)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Stackwright** plans for commit `%s`\n\n| Workspace | Status |\n| --- | --- |\n%s", commit[:min(7, len(commit))], table.String())
	token, err := g.installationToken(ctx, c.installation)
	if err != nil {
		return err
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT comment_id FROM github_comments WHERE repo_id = $1 AND pr_number = $2`, c.repoID, *c.pr).Scan(&id)
	switch {
	case err == nil:
		return g.call(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", c.repo, id), token, map[string]string{"body": b.String()}, nil)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := g.call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", c.repo, *c.pr), token, map[string]string{"body": b.String()}, &out); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO github_comments (repo_id, pr_number, org_id, comment_id) VALUES ($1, $2, $3, $4)`, c.repoID, *c.pr, orgID, out.ID)
	return err
}

package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/runs"
)

const (
	maxPayload  = 25 << 20
	maxAttempts = 5
	// compareLimit is the most files GitHub's compare API lists. A change
	// with more touches every project.
	compareLimit = 300
	zeroCommit   = "0000000000000000000000000000000000000000"
)

var handled = map[string]bool{"pull_request": true, "push": true, "installation": true, "installation_repositories": true}

// Webhook stores a delivery whose signature checks, and answers before any
// work. A redelivery has the same id and is stored once.
// https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries
func (g *GitHub) Webhook(w http.ResponseWriter, r *http.Request) {
	app, err := g.App(r.Context())
	if err != nil {
		http.Error(w, "no GitHub App", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
	if err != nil || len(body) > maxPayload {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	sig, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	got, err := hex.DecodeString(sig)
	mac := hmac.New(sha256.New, app.WebhookSecret)
	mac.Write(body)
	if !ok || err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if !handled[event] || delivery == "" || len(delivery) > 100 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := g.Pool.Exec(r.Context(), `INSERT INTO webhook_deliveries (delivery_id, event, payload) VALUES ($1, $2, $3)
		ON CONFLICT (delivery_id) DO NOTHING`, delivery, event, body); err != nil {
		slog.Error("store webhook", "delivery", delivery, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// RunWebhooks works through stored deliveries until ctx ends. A failure
// waits 30 s more on each attempt, five times.
func (g *GitHub) RunWebhooks(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := g.processDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("process webhooks", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (g *GitHub) processDue(ctx context.Context) error {
	if _, err := g.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < now() - interval '7 days'`); err != nil {
		return err
	}
	if _, err := g.Pool.Exec(ctx, `DELETE FROM github_states WHERE expires_at < now()`); err != nil {
		return err
	}
	rows, err := g.Pool.Query(ctx, `SELECT delivery_id, event, payload FROM webhook_deliveries
		WHERE processed_at IS NULL AND next_attempt_at <= now() ORDER BY received_at LIMIT 20`)
	if err != nil {
		return err
	}
	type delivery struct {
		id, event string
		payload   []byte
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (delivery, error) {
		var d delivery
		return d, r.Scan(&d.id, &d.event, &d.payload)
	})
	if err != nil {
		return err
	}
	for _, d := range due {
		perr := g.process(ctx, d.event, d.payload)
		if perr != nil {
			slog.Warn("webhook failed", "delivery", d.id, "event", d.event, "err", perr)
			_, err = g.Pool.Exec(ctx, `UPDATE webhook_deliveries SET attempts = attempts + 1, error = $2,
				next_attempt_at = now() + (attempts + 1) * interval '30 seconds',
				processed_at = CASE WHEN attempts + 1 >= $3 THEN now() END WHERE delivery_id = $1`, d.id, perr.Error(), maxAttempts)
		} else {
			_, err = g.Pool.Exec(ctx, `UPDATE webhook_deliveries SET processed_at = now(), error = NULL WHERE delivery_id = $1`, d.id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type repoRef struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type payload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository  repoRef `json:"repository"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA  string   `json:"sha"`
			Ref  string   `json:"ref"`
			Repo *repoRef `json:"repo"`
		} `json:"head"`
		Base struct {
			SHA  string  `json:"sha"`
			Ref  string  `json:"ref"`
			Repo repoRef `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func (g *GitHub) process(ctx context.Context, event string, raw []byte) error {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	var orgID string
	err := g.Pool.QueryRow(ctx, `SELECT org_id FROM github_installations WHERE installation_id = $1`, p.Installation.ID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		switch {
		case event == "installation" && p.Action == "deleted":
			_, err := tx.Exec(ctx, `DELETE FROM github_installations WHERE installation_id = $1`, p.Installation.ID)
			return err
		case event == "installation_repositories", event == "installation":
			return g.syncRepos(ctx, tx, orgID, p.Installation.ID)
		case event == "pull_request" && (p.Action == "opened" || p.Action == "synchronize" || p.Action == "reopened"):
			return g.pullRequest(ctx, tx, orgID, p)
		case event == "push":
			return g.push(ctx, tx, orgID, p)
		}
		return nil
	})
}

func (g *GitHub) repo(ctx context.Context, tx pgx.Tx, installation, githubID int64) (repoRow, error) {
	r := repoRow{installation: installation}
	err := tx.QueryRow(ctx, `SELECT id, full_name, default_branch FROM repos WHERE github_id = $1 AND installation_id = $2`,
		githubID, installation).Scan(&r.id, &r.fullName, &r.defaultBranch)
	return r, err
}

type target struct {
	projectID, folder string
	workspaces        []string
}

// targets returns the projects that apply from branch, with their live
// workspaces. A project with no branch applies from the default branch.
func targets(ctx context.Context, tx pgx.Tx, repo repoRow, branch string) (map[string]target, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, p.path, coalesce(array_agg(w.id::text ORDER BY w.name) FILTER (WHERE w.id IS NOT NULL), '{}')
		FROM projects p LEFT JOIN workspaces w ON w.project_id = p.id AND w.archived_at IS NULL
		WHERE p.archived_at IS NULL AND p.repo = lower($1) AND coalesce(nullif(p.branch, ''), $2) = $3
		GROUP BY p.id`, repo.fullName, repo.defaultBranch, branch)
	if err != nil {
		return nil, err
	}
	out := map[string]target{}
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.projectID, &t.folder, &t.workspaces); err != nil {
			rows.Close()
			return nil, err
		}
		if t.folder == "" {
			t.folder = "."
		}
		out[t.projectID] = t
	}
	return out, rows.Err()
}

// changedFiles lists the files between two commits, or nil when the list
// is unknown or longer than GitHub's compare API shows.
// https://docs.github.com/en/rest/commits/commits#compare-two-commits
func (g *GitHub) changedFiles(ctx context.Context, repo repoRow, base, head string) ([]string, error) {
	if base == "" || base == zeroCommit {
		return nil, nil
	}
	token, err := g.installationToken(ctx, repo.installation)
	if err != nil {
		return nil, err
	}
	var out struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := g.call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=%d", repo.fullName, base, head, compareLimit), token, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Files) >= compareLimit {
		return nil, nil
	}
	changed := []string{}
	for _, f := range out.Files {
		changed = append(changed, f.Filename)
		if f.PreviousFilename != "" {
			changed = append(changed, f.PreviousFilename)
		}
	}
	return changed, nil
}

func (g *GitHub) pullRequest(ctx context.Context, tx pgx.Tx, orgID string, p payload) error {
	pr := p.PullRequest
	repo, err := g.repo(ctx, tx, p.Installation.ID, pr.Base.Repo.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ts, err := targets(ctx, tx, repo, pr.Base.Ref)
	if err != nil || len(ts) == 0 {
		return err
	}
	if pr.Head.Repo == nil || pr.Head.Repo.ID != pr.Base.Repo.ID {
		return g.forkCheck(ctx, repo, pr.Head.SHA)
	}
	changed, err := g.changedFiles(ctx, repo, pr.Base.SHA, pr.Head.SHA)
	if err != nil {
		return err
	}
	return g.start(ctx, tx, orgID, repo, ts, changed, runs.NewRun{
		PlanOnly: true, Trigger: "pull_request", CommitSHA: pr.Head.SHA, Branch: pr.Head.Ref, PRNumber: pr.Number,
		SourceName: fmt.Sprintf("%s#%d", repo.fullName, pr.Number),
	})
}

func (g *GitHub) push(ctx context.Context, tx pgx.Tx, orgID string, p payload) error {
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || p.After == zeroCommit {
		return nil
	}
	repo, err := g.repo(ctx, tx, p.Installation.ID, p.Repository.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ts, err := targets(ctx, tx, repo, branch)
	if err != nil || len(ts) == 0 {
		return err
	}
	changed, err := g.changedFiles(ctx, repo, p.Before, p.After)
	if err != nil {
		return err
	}
	return g.start(ctx, tx, orgID, repo, ts, changed, runs.NewRun{
		Trigger: "push", CommitSHA: p.After, Branch: branch, SourceName: fmt.Sprintf("%s@%s", repo.fullName, branch),
	})
}

// start queues a run for each workspace of each project that the change
// touches, once for each commit and pull request.
func (g *GitHub) start(ctx context.Context, tx pgx.Tx, orgID string, repo repoRow, ts map[string]target, changed []string, base runs.NewRun) error {
	a, err := g.archive(ctx, tx, orgID, repo, base.CommitSHA)
	if err != nil {
		return err
	}
	folders := map[string]string{}
	for id, t := range ts {
		folders[id] = t.folder
	}
	for id := range affected(folders, changed, a.files) {
		for _, ws := range ts[id].workspaces {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM runs WHERE workspace_id = $1 AND trigger = $2 AND commit_sha = $3
				AND coalesce(pr_number, 0) = $4)`, ws, base.Trigger, base.CommitSHA, base.PRNumber).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			r := base
			r.WorkspaceID, r.SourceKey, r.SourceSHA, r.RepoID = ws, a.key, a.sha256, repo.id
			runID, _, err := g.Runs.Create(ctx, tx, orgID, r)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO github_checks (run_id, org_id) VALUES ($1, $2)`, runID, orgID); err != nil {
				return err
			}
		}
	}
	return nil
}

package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/notify"
)

// ErrCycle is a deployment whose links loop.
var ErrCycle = errors.New("the deployment's links make a cycle")

// Step is a deployment step: a workspace and the steps it follows.
type Step struct {
	WorkspaceID string
	Name        string // "project/workspace"
	After       []string
	// Destroy is a step that plans with -destroy in every run: a Stack's
	// removed instance (adr/0043).
	Destroy bool
}

// DeploymentOrder returns a deployment's steps in the order they apply:
// each after the steps it links from, and by name among equals. tx needs
// the org setting.
func DeploymentOrder(ctx context.Context, tx pgx.Tx, deploymentID string) ([]Step, error) {
	rows, err := tx.Query(ctx, `SELECT s.workspace_id, p.slug || '/' || w.name, s.destroy FROM deployment_steps s
		JOIN workspaces w ON w.id = s.workspace_id JOIN projects p ON p.id = w.project_id WHERE s.deployment_id = $1`, deploymentID)
	if err != nil {
		return nil, err
	}
	steps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Step, error) {
		var s Step
		return s, r.Scan(&s.WorkspaceID, &s.Name, &s.Destroy)
	})
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT from_workspace_id, to_workspace_id FROM deployment_links WHERE deployment_id = $1`, deploymentID)
	if err != nil {
		return nil, err
	}
	edges, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var e [2]string
		return e, r.Scan(&e[0], &e[1])
	})
	if err != nil {
		return nil, err
	}
	return Order(steps, edges)
}

// Order sorts steps so each comes after the steps it follows, by name
// among equals, or returns ErrCycle.
func Order(steps []Step, edges [][2]string) ([]Step, error) {
	byID := map[string]*Step{}
	for i := range steps {
		byID[steps[i].WorkspaceID] = &steps[i]
	}
	for _, e := range edges {
		if s := byID[e[1]]; s != nil && byID[e[0]] != nil {
			s.After = append(s.After, e[0])
		}
	}
	var out []Step
	done := map[string]bool{}
	for len(out) < len(steps) {
		var ready []Step
		for _, s := range steps {
			if !done[s.WorkspaceID] && !slices.ContainsFunc(s.After, func(id string) bool { return !done[id] }) {
				ready = append(ready, s)
			}
		}
		if len(ready) == 0 {
			return nil, ErrCycle
		}
		slices.SortFunc(ready, func(a, b Step) int { return strings.Compare(a.Name, b.Name) })
		for _, s := range ready {
			done[s.WorkspaceID] = true
			out = append(out, *byID[s.WorkspaceID])
		}
	}
	return out, nil
}

// output is one value of `output -json`.
type output struct {
	Sensitive bool            `json:"sensitive"`
	Value     json.RawMessage `json:"value"`
}

// inputs are a step's variables from earlier steps' outputs, as JSON
// JobVariables, and the outputs that are missing.
func (o *Orchestrator) inputs(ctx context.Context, tx pgx.Tx, orgID, deploymentID, workspaceID string) ([]byte, []string, error) {
	if b, missing, ok, err := o.stackInputs(ctx, tx, orgID, workspaceID); ok || err != nil {
		return b, missing, err
	}
	rows, err := tx.Query(ctx, `SELECT l.from_workspace_id, p.slug || '/' || w.name, l.output, l.variable FROM deployment_links l
		JOIN workspaces w ON w.id = l.from_workspace_id JOIN projects p ON p.id = w.project_id
		WHERE l.deployment_id = $1 AND l.to_workspace_id = $2 AND l.output <> '' ORDER BY l.variable`, deploymentID, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	type link struct{ from, name, output, variable string }
	links, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (link, error) {
		var l link
		return l, r.Scan(&l.from, &l.name, &l.output, &l.variable)
	})
	if err != nil {
		return nil, nil, err
	}
	outputs := map[string]map[string]output{}
	vars := []*v1.JobVariable{}
	var missing []string
	for _, l := range links {
		if _, ok := outputs[l.from]; !ok {
			var sealed []byte
			err := tx.QueryRow(ctx, `SELECT outputs FROM workspace_outputs WHERE workspace_id = $1`, l.from).Scan(&sealed)
			outs := map[string]output{}
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return nil, nil, err
			default:
				plain, err := o.KEK.Open(ctx, tx, orgID, sealed)
				if err != nil {
					return nil, nil, err
				}
				if err := json.Unmarshal(plain, &outs); err != nil {
					return nil, nil, fmt.Errorf("the outputs of %s: %w", l.name, err)
				}
			}
			outputs[l.from] = outs
		}
		out, ok := outputs[l.from][l.output]
		if !ok {
			missing = append(missing, l.name+"."+l.output)
			continue
		}
		// A string goes as itself; any other type as JSON, which Terraform
		// reads for a complex variable.
		value := string(out.Value)
		var str string
		if json.Unmarshal(out.Value, &str) == nil {
			value = str
		}
		vars = append(vars, &v1.JobVariable{Category: v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM, Key: l.variable, Value: value,
			Sensitive: out.Sensitive})
	}
	b, err := json.Marshal(vars)
	return b, missing, err
}

// StartDeployment queues a deployment run, and plans each step whose
// linked outputs exist. The others plan when the steps before them have
// applied. tx needs the org setting.
// code is the commit every step plans; nil plans each step's newest code.
func (o *Orchestrator) StartDeployment(ctx context.Context, tx pgx.Tx, orgID, deploymentID, userID string, destroy bool, code *NewRun) (string, int32, error) {
	steps, err := DeploymentOrder(ctx, tx, deploymentID)
	if err != nil {
		return "", 0, err
	}
	if len(steps) == 0 {
		return "", 0, errors.New("the deployment has no steps")
	}
	id := uuid.NewString()
	var number int32
	var codeJSON []byte
	if code != nil {
		if codeJSON, err = json.Marshal(code); err != nil {
			return "", 0, err
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO deployment_runs (id, org_id, deployment_id, number, destroy, created_by, code)
		SELECT $1, $2, $3, coalesce(max(number), 0) + 1, $4, nullif($5, '')::uuid, $6 FROM deployment_runs WHERE deployment_id = $3
		RETURNING number`, id, orgID, deploymentID, destroy, userID, codeJSON).Scan(&number); err != nil {
		return "", 0, err
	}
	for _, s := range steps {
		if _, err := o.startStep(ctx, tx, orgID, deploymentID, id, s, destroy, userID, destroy); err != nil {
			return "", 0, err
		}
	}
	o.Wake()
	return id, number, nil
}

// startStep plans a step, unless an output it needs is missing and
// waiting is allowed: then it reports false.
func (o *Orchestrator) startStep(ctx context.Context, tx pgx.Tx, orgID, deploymentID, runID string, s Step, destroy bool, userID string,
	mustStart bool) (bool, error) {
	inputs, missing, err := o.inputs(ctx, tx, orgID, deploymentID, s.WorkspaceID)
	if err != nil {
		return false, err
	}
	if len(missing) > 0 && !mustStart {
		return false, nil
	}
	var code *NewRun
	if err := tx.QueryRow(ctx, `SELECT code FROM deployment_runs WHERE id = $1`, runID).Scan(&code); err != nil {
		return false, err
	}
	var r NewRun
	if code != nil {
		r = *code
		r.WorkspaceID = s.WorkspaceID
	} else {
		var ok bool
		r, ok, err = codeOf(ctx, tx, orgID, s.WorkspaceID, o.Head)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%s has never run its own code; run it once, then deploy", s.Name)
		}
	}
	r.Trigger, r.Destroy, r.Inputs, r.DeploymentRunID, r.CreatedBy = "deployment", destroy || s.Destroy, inputs, runID, userID
	_, _, err = o.Create(ctx, tx, orgID, r)
	return err == nil, err
}

type stepRun struct {
	id, status, errMsg string
	hasChanges         bool
	inputs             []byte
}

// latestSteps returns each step's newest run in a deployment run.
func latestSteps(ctx context.Context, tx pgx.Tx, deploymentRunID string) (map[string]stepRun, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (workspace_id) workspace_id, id, status, coalesce(error, ''), coalesce(has_changes, false), inputs
		FROM runs WHERE deployment_run_id = $1 ORDER BY workspace_id, number DESC`, deploymentRunID)
	if err != nil {
		return nil, err
	}
	type row struct {
		workspace string
		run       stepRun
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.workspace, &x.run.id, &x.run.status, &x.run.errMsg, &x.run.hasChanges, &x.run.inputs)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := map[string]stepRun{}
	for _, x := range all {
		out[x.workspace] = x.run
	}
	return out, nil
}

func (s stepRun) done() bool {
	return s.status == "applied" || s.status == "planned" && !s.hasChanges
}

func (s stepRun) ended() bool {
	return slices.Contains([]string{"failed", "canceled", "rejected"}, s.status)
}

// advanceDeployments moves every active deployment run on.
func (o *Orchestrator) advanceDeployments(ctx context.Context) error {
	rows, err := o.Pool.Query(ctx, `SELECT deployment_run_id, org_id FROM active_deployment_runs()`)
	if err != nil {
		return err
	}
	type active struct{ id, org string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (active, error) {
		var a active
		return a, r.Scan(&a.id, &a.org)
	})
	if err != nil {
		return err
	}
	for _, a := range all {
		if err := db.InOrg(ctx, o.Pool, a.org, func(tx pgx.Tx) error { return o.advance(ctx, tx, a.org, a.id) }); err != nil && ctx.Err() == nil {
			slog.Error("advance deployment run", "deployment_run", a.id, "err", err)
		}
	}
	return nil
}

// advance moves one deployment run on: from planning to its approval, and
// while applying, each step in order when the steps before it are done.
func (o *Orchestrator) advance(ctx context.Context, tx pgx.Tx, orgID, id string) error {
	var deploymentID, status string
	var destroy bool
	var decidedBy *string
	// NO KEY UPDATE: a step's run that updates itself twice in one
	// transaction re-checks its foreign key with KEY SHARE on this row,
	// which FOR UPDATE would block while this waits for that run.
	if err := tx.QueryRow(ctx, `SELECT deployment_id, status, destroy, decided_by::text FROM deployment_runs WHERE id = $1 FOR NO KEY UPDATE`, id).
		Scan(&deploymentID, &status, &destroy, &decidedBy); err != nil {
		return err
	}
	steps, err := DeploymentOrder(ctx, tx, deploymentID)
	if err != nil {
		return err
	}
	if destroy {
		slices.Reverse(steps)
	}
	runs, err := latestSteps(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if r, ok := runs[s.WorkspaceID]; ok && r.ended() {
			return o.failDeployment(ctx, tx, id, steps, runs, fmt.Sprintf("%s %s: %s", s.Name, r.status, r.errMsg), "")
		}
	}

	switch status {
	case "planning":
		waiting := false
		for _, s := range steps {
			r, ok := runs[s.WorkspaceID]
			if ok && slices.Contains([]string{"queued", "planning"}, r.status) {
				return nil
			}
			waiting = waiting || !ok || !r.done()
		}
		next := "needs_approval"
		if !waiting {
			next = "planned"
		}
		return setDeploymentStatus(ctx, tx, id, next, "")
	case "applying":
		approver := ""
		if decidedBy != nil {
			approver = *decidedBy
		}
		before := map[string]bool{}
		allDone := true
		for _, s := range steps {
			r, ok := runs[s.WorkspaceID]
			if ok && r.done() {
				before[s.WorkspaceID] = true
				continue
			}
			allDone = false
			// A destroy goes in reverse: a step waits for those that
			// follow it.
			deps := s.After
			if destroy {
				deps = nil
				for _, t := range steps {
					if slices.Contains(t.After, s.WorkspaceID) {
						deps = append(deps, t.WorkspaceID)
					}
				}
			}
			if slices.ContainsFunc(deps, func(d string) bool { return !before[d] }) {
				continue
			}
			if !ok {
				if _, err := o.startStep(ctx, tx, orgID, deploymentID, id, s, destroy, approver, true); err != nil {
					return o.failDeployment(ctx, tx, id, steps, runs, s.Name+": "+err.Error(), "")
				}
				continue
			}
			if r.status != "needs_approval" {
				continue
			}
			// An earlier step's apply may have changed the outputs this
			// step planned with: then it plans again.
			fresh, missing, err := o.inputs(ctx, tx, orgID, deploymentID, s.WorkspaceID)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				return o.failDeployment(ctx, tx, id, steps, runs, fmt.Sprintf("%s needs %s, which no apply has made", s.Name,
					strings.Join(missing, ", ")), "")
			}
			planned, err := o.openInputs(ctx, tx, orgID, r.inputs)
			if err != nil {
				return err
			}
			if !bytes.Equal(planned, fresh) {
				if err := o.cancel(ctx, tx, r.id, ""); err != nil {
					return err
				}
				if _, err := o.startStep(ctx, tx, orgID, deploymentID, id, s, destroy, approver, true); err != nil {
					return err
				}
				continue
			}
			if err := o.approve(ctx, tx, r.id, approver); err != nil {
				return err
			}
		}
		if allDone {
			// A Stack's removed instance is gone once its destroy applies
			// (adr/0043).
			for _, s := range steps {
				if !s.Destroy || runs[s.WorkspaceID].status != "applied" {
					continue
				}
				for _, q := range []string{`UPDATE workspaces SET archived_at = now() WHERE id = $1`,
					`DELETE FROM stack_instances WHERE workspace_id = $1`,
					`DELETE FROM deployment_steps WHERE workspace_id = $1`} {
					if _, err := tx.Exec(ctx, q, s.WorkspaceID); err != nil {
						return err
					}
				}
			}
			return setDeploymentStatus(ctx, tx, id, "applied", "")
		}
	}
	return nil
}

func (o *Orchestrator) openInputs(ctx context.Context, tx pgx.Tx, orgID string, sealed []byte) ([]byte, error) {
	if sealed == nil {
		return []byte("[]"), nil
	}
	return o.KEK.Open(ctx, tx, orgID, sealed)
}

func setDeploymentStatus(ctx context.Context, tx pgx.Tx, id, status, msg string) error {
	var was string
	if err := tx.QueryRow(ctx, `SELECT status FROM deployment_runs WHERE id = $1`, id).Scan(&was); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE deployment_runs SET status = $2, error = nullif($3, ''), updated_at = now(),
		finished_at = CASE WHEN $2 IN ('applied', 'planned', 'failed', 'rejected', 'canceled') THEN now() END WHERE id = $1`, id, status, msg); err != nil {
		return err
	}
	event, ok := map[string]string{"failed": notify.RunFailed, "applied": notify.RunApplied, "needs_approval": notify.RunNeedsApproval}[status]
	if !ok || was == status {
		return nil
	}
	e := notify.Event{Type: event, Error: msg}
	var orgID string
	var number int
	if err := tx.QueryRow(ctx, `SELECT r.org_id, o.slug, d.name, r.number FROM deployment_runs r
		JOIN deployments d ON d.id = r.deployment_id JOIN orgs o ON o.id = r.org_id WHERE r.id = $1`, id).
		Scan(&orgID, &e.Org, &e.Deployment, &number); err != nil {
		return err
	}
	where := fmt.Sprintf("Deployment %s, run #%d,", e.Deployment, number)
	switch event {
	case notify.RunFailed:
		e.Text = "✗ " + where + " failed: " + msg
	case notify.RunApplied:
		e.Text = "✓ " + where + " applied"
	default:
		e.Text = where + " waits for approval"
	}
	return notify.Enqueue(ctx, tx, orgID, e)
}

// failDeployment ends a deployment run and every step run that has not
// finished: the steps after a failure do not apply, and those applied
// stay applied.
func (o *Orchestrator) failDeployment(ctx context.Context, tx pgx.Tx, id string, steps []Step, runs map[string]stepRun, msg, userID string) error {
	return o.endDeployment(ctx, tx, id, steps, runs, "failed", msg, userID)
}

func (o *Orchestrator) endDeployment(ctx context.Context, tx pgx.Tx, id string, steps []Step, runs map[string]stepRun, status, msg,
	userID string) error {
	for _, s := range steps {
		r, ok := runs[s.WorkspaceID]
		if !ok || r.done() || r.ended() || r.status == "planned" || r.status == "applying" {
			continue
		}
		if err := o.cancel(ctx, tx, r.id, userID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE deployment_runs SET decided_by = coalesce(nullif($2, '')::uuid, decided_by) WHERE id = $1`, id,
		userID); err != nil {
		return err
	}
	return setDeploymentStatus(ctx, tx, id, status, msg)
}

// DecideDeployment approves, rejects or cancels a deployment run. Approve
// needs every step planned and none blocked. tx needs the org setting.
func (o *Orchestrator) DecideDeployment(ctx context.Context, tx pgx.Tx, id, userID, decision string) error {
	var deploymentID, status string
	if err := tx.QueryRow(ctx, `SELECT deployment_id, status FROM deployment_runs WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&deploymentID, &status); err != nil {
		return err
	}
	steps, err := DeploymentOrder(ctx, tx, deploymentID)
	if err != nil {
		return err
	}
	runs, err := latestSteps(ctx, tx, id)
	if err != nil {
		return err
	}
	switch decision {
	case "approve":
		if status != "needs_approval" {
			return refuse(connect.CodeFailedPrecondition, "the deployment run is %s, so it waits for no approval", status)
		}
		for _, s := range steps {
			if r, ok := runs[s.WorkspaceID]; ok && r.status == "blocked" {
				return refuse(connect.CodeFailedPrecondition, "a security finding blocks %s; fix it or accept its risk first", s.Name)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE deployment_runs SET status = 'applying', decided_by = $2, updated_at = now() WHERE id = $1`, id,
			userID); err != nil {
			return err
		}
		o.Wake()
		return nil
	case "reject":
		if status != "needs_approval" {
			return refuse(connect.CodeFailedPrecondition, "the deployment run is %s, so it waits for no approval", status)
		}
		return o.endDeployment(ctx, tx, id, steps, runs, "rejected", "", userID)
	default:
		if !slices.Contains([]string{"planning", "needs_approval", "applying"}, status) {
			return refuse(connect.CodeFailedPrecondition, "the deployment run is %s already", status)
		}
		return o.endDeployment(ctx, tx, id, steps, runs, "canceled", "", userID)
	}
}

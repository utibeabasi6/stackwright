package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/authz"
	"github.com/utibeabasi6/stackwright/internal/db"
	"github.com/utibeabasi6/stackwright/internal/runs"
)

const maxDeploymentSteps = 50

// Deployments serves deployments: several workspaces that apply in order
// with one approval (adr/0042).
type Deployments struct {
	stackwrightv1connect.UnimplementedDeploymentServiceHandler
	Orchestrator *runs.Orchestrator
}

var deploymentStatuses = map[string]v1.DeploymentRunStatus{
	"planning": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_PLANNING, "needs_approval": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL,
	"applying": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_APPLYING, "applied": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_APPLIED,
	"planned": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_PLANNED, "failed": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_FAILED,
	"rejected": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_REJECTED, "canceled": v1.DeploymentRunStatus_DEPLOYMENT_RUN_STATUS_CANCELED,
}

func deploymentID(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM deployments WHERE name = $1 AND archived_at IS NULL`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", authz.Internal(err)
	}
	return id, nil
}

// notStackDefined refuses to change a deployment that a Stack's files
// define (adr/0043).
func notStackDefined(ctx context.Context, tx pgx.Tx, id string) error {
	var project string
	if err := tx.QueryRow(ctx, `SELECT coalesce(p.slug, '') FROM deployments d LEFT JOIN projects p ON p.id = d.project_id WHERE d.id = $1`,
		id).Scan(&project); err != nil {
		return authz.Internal(err)
	}
	if project != "" {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the Stack files of project %s define this deployment; change them instead", project))
	}
	return nil
}

func refOf(name string) *v1.WorkspaceRef {
	i := strings.LastIndexByte(name, '/')
	return &v1.WorkspaceRef{Project: name[:i], Workspace: name[i+1:]}
}

func loadDeployment(ctx context.Context, tx pgx.Tx, id, name string) (*v1.Deployment, error) {
	steps, err := runs.DeploymentOrder(ctx, tx, id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	d := &v1.Deployment{Name: name}
	if err := tx.QueryRow(ctx, `SELECT coalesce(p.slug, ''), coalesce(d.stack_deployment, ''), coalesce(d.stack_group, '') FROM deployments d
		LEFT JOIN projects p ON p.id = d.project_id WHERE d.id = $1`, id).Scan(&d.StackProject, &d.StackDeployment, &d.StackGroup); err != nil {
		return nil, authz.Internal(err)
	}
	for _, s := range steps {
		d.Steps = append(d.Steps, refOf(s.Name))
	}
	rows, err := tx.Query(ctx, `SELECT pf.slug || '/' || wf.name, pt.slug || '/' || wt.name, l.output, l.variable FROM deployment_links l
		JOIN workspaces wf ON wf.id = l.from_workspace_id JOIN projects pf ON pf.id = wf.project_id
		JOIN workspaces wt ON wt.id = l.to_workspace_id JOIN projects pt ON pt.id = wt.project_id
		WHERE l.deployment_id = $1 ORDER BY 1, 2, 4`, id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if d.Links, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.DeploymentLink, error) {
		var from, to string
		l := &v1.DeploymentLink{}
		err := r.Scan(&from, &to, &l.Output, &l.Variable)
		l.From, l.To = refOf(from), refOf(to)
		return l, err
	}); err != nil {
		return nil, authz.Internal(err)
	}
	runs, err := deploymentRuns(ctx, tx, id, 0, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) > 0 {
		d.LastRun = runs[0]
	}
	return d, nil
}

// deploymentRuns returns a deployment's runs, newest first, or the one
// numbered number.
func deploymentRuns(ctx context.Context, tx pgx.Tx, deploymentID string, number int32, limit int) ([]*v1.DeploymentRun, error) {
	rows, err := tx.Query(ctx, `SELECT r.id, r.number, r.status, r.destroy, coalesce(r.error, ''), coalesce(c.name, c.email, ''),
		coalesce(d.name, d.email, ''), r.created_at, r.finished_at FROM deployment_runs r
		LEFT JOIN users c ON c.id = r.created_by LEFT JOIN users d ON d.id = r.decided_by
		WHERE r.deployment_id = $1 AND ($2 = 0 OR r.number = $2) ORDER BY r.number DESC LIMIT $3`, deploymentID, number, limit)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type row struct {
		id  string
		run *v1.DeploymentRun
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{run: &v1.DeploymentRun{}}
		var status string
		var created time.Time
		var finished *time.Time
		err := r.Scan(&x.id, &x.run.Number, &status, &x.run.Destroy, &x.run.Error, &x.run.CreatedBy, &x.run.DecidedBy, &created, &finished)
		x.run.Status, x.run.CreatedAt = deploymentStatuses[status], timestamppb.New(created)
		if finished != nil {
			x.run.FinishedAt = timestamppb.New(*finished)
		}
		return x, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	steps, err := runs.DeploymentOrder(ctx, tx, deploymentID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := make([]*v1.DeploymentRun, 0, len(all))
	for _, x := range all {
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (workspace_id) workspace_id, number, status, coalesce(has_changes, false)
			FROM runs WHERE deployment_run_id = $1 ORDER BY workspace_id, number DESC`, x.id)
		if err != nil {
			return nil, authz.Internal(err)
		}
		type latest struct {
			workspace  string
			number     int32
			status     string
			hasChanges bool
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (latest, error) {
			var l latest
			return l, r.Scan(&l.workspace, &l.number, &l.status, &l.hasChanges)
		})
		if err != nil {
			return nil, authz.Internal(err)
		}
		byWS := map[string]latest{}
		for _, l := range list {
			byWS[l.workspace] = l
		}
		order := steps
		if x.run.Destroy {
			order = slices.Clone(steps)
			slices.Reverse(order)
		}
		done := func(id string) bool {
			l, ok := byWS[id]
			return ok && (l.status == "applied" || l.status == "planned" && !l.hasChanges)
		}
		for _, s := range order {
			step := &v1.DeploymentStep{Workspace: refOf(s.Name)}
			if l, ok := byWS[s.WorkspaceID]; ok {
				step.RunNumber, step.RunStatus, step.HasChanges = l.number, v1.RunStatus(v1.RunStatus_value["RUN_STATUS_"+strings.ToUpper(l.status)]), l.hasChanges
			}
			for _, a := range s.After {
				if !done(a) {
					for _, t := range steps {
						if t.WorkspaceID == a {
							step.WaitingFor = append(step.WaitingFor, refOf(t.Name))
						}
					}
				}
			}
			x.run.Steps = append(x.run.Steps, step)
		}
		out = append(out, x.run)
	}
	return out, nil
}

func (Deployments) ListDeployments(ctx context.Context, _ *connect.Request[v1.ListDeploymentsRequest]) (*connect.Response[v1.ListDeploymentsResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	rows, err := tx.Query(ctx, `SELECT id, name FROM deployments WHERE archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type row struct{ id, name string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.name)
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := &v1.ListDeploymentsResponse{}
	for _, x := range all {
		d, err := loadDeployment(ctx, tx, x.id, x.name)
		if err != nil {
			return nil, err
		}
		out.Deployments = append(out.Deployments, d)
	}
	return connect.NewResponse(out), nil
}

func (Deployments) GetDeployment(ctx context.Context, req *connect.Request[v1.GetDeploymentRequest]) (*connect.Response[v1.GetDeploymentResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	id, err := deploymentID(ctx, tx, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	d, err := loadDeployment(ctx, tx, id, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	all, err := deploymentRuns(ctx, tx, id, 0, 20)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetDeploymentResponse{Deployment: d, Runs: all}), nil
}

func (Deployments) SaveDeployment(ctx context.Context, req *connect.Request[v1.SaveDeploymentRequest]) (*connect.Response[v1.SaveDeploymentResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if !slugPattern.MatchString(m.Name) {
		return nil, invalid("a deployment's name needs 1 to 40 lowercase letters, digits or hyphens")
	}
	if len(m.Steps) == 0 || len(m.Steps) > maxDeploymentSteps {
		return nil, invalid(fmt.Sprintf("a deployment has 1 to %d steps", maxDeploymentSteps))
	}
	ids := map[string]string{}
	key := func(r *v1.WorkspaceRef) string { return r.GetProject() + "/" + r.GetWorkspace() }
	for _, st := range m.Steps {
		ws, err := workspaceID(ctx, s.Tx, st.Project, st.Workspace)
		if errors.Is(err, errNotFound) {
			return nil, invalid(fmt.Sprintf("%s is not a workspace", key(st)))
		}
		if err != nil {
			return nil, err
		}
		if _, dup := ids[key(st)]; dup {
			return nil, invalid(fmt.Sprintf("%s is a step twice", key(st)))
		}
		ids[key(st)] = ws
	}
	var edges [][2]string
	for _, l := range m.Links {
		from, to := ids[key(l.From)], ids[key(l.To)]
		if from == "" || to == "" {
			return nil, invalid(fmt.Sprintf("the link from %s to %s names a workspace that is not a step", key(l.From), key(l.To)))
		}
		if from == to {
			return nil, invalid(fmt.Sprintf("%s cannot follow itself", key(l.From)))
		}
		if (l.Output == "") != (l.Variable == "") {
			return nil, invalid("a link names both an output and a variable, or neither")
		}
		edges = append(edges, [2]string{from, to})
	}
	steps := make([]runs.Step, 0, len(ids))
	for name, id := range ids {
		steps = append(steps, runs.Step{WorkspaceID: id, Name: name})
	}
	if _, err := runs.Order(steps, edges); errors.Is(err, runs.ErrCycle) {
		return nil, invalid("the links make a cycle: each step must come after the steps it gets outputs from")
	}

	id, err := deploymentID(ctx, s.Tx, m.Name)
	switch {
	case errors.Is(err, errNotFound):
		id = uuid.NewString()
		if _, err := s.Tx.Exec(ctx, `INSERT INTO deployments (id, org_id, name, created_by) VALUES ($1, app_org_id(), $2, $3)`, id, m.Name,
			s.UserID); err != nil {
			return nil, authz.Internal(err)
		}
	case err != nil:
		return nil, err
	default:
		if err := notStackDefined(ctx, s.Tx, id); err != nil {
			return nil, err
		}
		var active bool
		if err := s.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM deployment_runs WHERE deployment_id = $1
			AND status IN ('planning', 'needs_approval', 'applying'))`, id).Scan(&active); err != nil {
			return nil, authz.Internal(err)
		}
		if active {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a run of this deployment is under way; change it when the run ends"))
		}
		if _, err := s.Tx.Exec(ctx, `DELETE FROM deployment_steps WHERE deployment_id = $1`, id); err != nil {
			return nil, authz.Internal(err)
		}
	}
	for _, ws := range ids {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO deployment_steps (deployment_id, org_id, workspace_id) VALUES ($1, app_org_id(), $2)`, id,
			ws); err != nil {
			return nil, authz.Internal(err)
		}
	}
	for _, l := range m.Links {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO deployment_links (deployment_id, org_id, from_workspace_id, to_workspace_id, output, variable)
			VALUES ($1, app_org_id(), $2, $3, $4, $5) ON CONFLICT DO NOTHING`, id, ids[key(l.From)], ids[key(l.To)], l.Output, l.Variable); err != nil {
			return nil, invalid(fmt.Sprintf("the link %s.%s to %s.%s: %v", key(l.From), l.Output, key(l.To), l.Variable, err))
		}
	}
	s.Target("deployment " + m.Name)
	d, err := loadDeployment(ctx, s.Tx, id, m.Name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.SaveDeploymentResponse{Deployment: d}), nil
}

func (Deployments) ArchiveDeployment(ctx context.Context, req *connect.Request[v1.ArchiveDeploymentRequest]) (*connect.Response[v1.ArchiveDeploymentResponse], error) {
	s := authz.ScopeFrom(ctx)
	id, err := deploymentID(ctx, s.Tx, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	if err := notStackDefined(ctx, s.Tx, id); err != nil {
		return nil, err
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE deployments SET archived_at = now() WHERE id = $1`, id); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("deployment " + req.Msg.Name)
	return connect.NewResponse(&v1.ArchiveDeploymentResponse{}), nil
}

func (h Deployments) StartDeploymentRun(ctx context.Context, req *connect.Request[v1.StartDeploymentRunRequest]) (*connect.Response[v1.StartDeploymentRunResponse], error) {
	s := authz.ScopeFrom(ctx)
	id, err := deploymentID(ctx, s.Tx, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	// Every component of a Stack plans the same commit (adr/0043).
	var code *runs.NewRun
	var first string
	if err := s.Tx.QueryRow(ctx, `SELECT coalesce(min(st.workspace_id::text), '') FROM deployments d JOIN deployment_steps st
		ON st.deployment_id = d.id WHERE d.id = $1 AND d.project_id IS NOT NULL`, id).Scan(&first); err != nil {
		return nil, authz.Internal(err)
	}
	if first != "" && h.Orchestrator.Head != nil {
		r, linked, err := h.Orchestrator.Head(ctx, s.Tx, s.OrgID, first)
		if err != nil {
			return nil, githubError(err)
		}
		if linked {
			code = &r
		}
	}
	_, number, err := h.Orchestrator.StartDeployment(ctx, s.Tx, s.OrgID, id, s.UserID, req.Msg.Destroy, code)
	switch {
	case db.UniqueViolation(err):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a run of this deployment is under way"))
	case err != nil:
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.Target(fmt.Sprintf("deployment %s run #%d", req.Msg.Name, number))
	all, err := deploymentRuns(ctx, s.Tx, id, number, 1)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StartDeploymentRunResponse{Run: all[0]}), nil
}

func (h Deployments) decide(ctx context.Context, name string, number int32, decision string) (*v1.DeploymentRun, error) {
	s := authz.ScopeFrom(ctx)
	id, err := deploymentID(ctx, s.Tx, name)
	if err != nil {
		return nil, err
	}
	var runID string
	err = s.Tx.QueryRow(ctx, `SELECT id FROM deployment_runs WHERE deployment_id = $1 AND number = $2`, id, number).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if err := h.Orchestrator.DecideDeployment(ctx, s.Tx, runID, s.UserID, decision); err != nil {
		if ce := new(connect.Error); errors.As(err, &ce) {
			return nil, ce
		}
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("deployment %s run #%d", name, number))
	all, err := deploymentRuns(ctx, s.Tx, id, number, 1)
	if err != nil {
		return nil, err
	}
	return all[0], nil
}

func (h Deployments) ApproveDeploymentRun(ctx context.Context, req *connect.Request[v1.ApproveDeploymentRunRequest]) (*connect.Response[v1.ApproveDeploymentRunResponse], error) {
	run, err := h.decide(ctx, req.Msg.Name, req.Msg.Number, "approve")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ApproveDeploymentRunResponse{Run: run}), nil
}

func (h Deployments) RejectDeploymentRun(ctx context.Context, req *connect.Request[v1.RejectDeploymentRunRequest]) (*connect.Response[v1.RejectDeploymentRunResponse], error) {
	run, err := h.decide(ctx, req.Msg.Name, req.Msg.Number, "reject")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.RejectDeploymentRunResponse{Run: run}), nil
}

func (h Deployments) CancelDeploymentRun(ctx context.Context, req *connect.Request[v1.CancelDeploymentRunRequest]) (*connect.Response[v1.CancelDeploymentRunResponse], error) {
	run, err := h.decide(ctx, req.Msg.Name, req.Msg.Number, "cancel")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CancelDeploymentRunResponse{Run: run}), nil
}

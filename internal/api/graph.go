package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/graph"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/topology"
)

const (
	maxLayoutNodes = 5000
	maxNodeID      = 1024
)

type Graph struct {
	Jobs *jobs.Service
	// GitHubURL is the web address of GitHub, for links to files.
	GitHubURL string
}

type planArtifacts struct {
	number      int32
	plan        []byte
	sources     []byte
	workspaceID string
	baseURL     string
}

// load reads a run's plan.json and sources.json. Number 0 means the newest
// run whose plan succeeded.
func (h Graph) load(ctx context.Context, tx pgx.Tx, project, workspace string, number int32) (*planArtifacts, error) {
	ws, err := workspaceID(ctx, tx, project, workspace)
	if err != nil {
		return nil, err
	}
	a := &planArtifacts{workspaceID: ws}
	var jobID, status, sha, repo string
	err = tx.QueryRow(ctx, `SELECT r.number, j.id, j.status, coalesce(r.commit_sha, ''), coalesce(rp.full_name, '')
		FROM runs r JOIN jobs j ON j.id = r.plan_job_id LEFT JOIN repos rp ON rp.id = r.repo_id
		WHERE r.workspace_id = $1 AND ($2 = 0 OR r.number = $2) AND ($2 <> 0 OR j.status = 'succeeded')
		ORDER BY r.number DESC LIMIT 1`, ws, number).Scan(&a.number, &jobID, &status, &sha, &repo)
	if errors.Is(err, pgx.ErrNoRows) {
		if number == 0 {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no run in this workspace has a plan yet"))
		}
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if status != "succeeded" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run #%d has no plan to show", a.number))
	}
	if a.plan, err = h.Jobs.ReadArtifact(ctx, tx, jobID, "plan.json"); err != nil {
		return nil, authz.Internal(err)
	}
	a.sources, err = h.Jobs.ReadArtifact(ctx, tx, jobID, "sources.json")
	if err != nil && !errors.Is(err, jobs.ErrNoArtifact) {
		return nil, authz.Internal(err)
	}
	if repo != "" && sha != "" {
		a.baseURL = fmt.Sprintf("%s/%s/blob/%s/", h.GitHubURL, repo, sha)
	}
	return a, nil
}

func (h Graph) GetPlanGraph(ctx context.Context, req *connect.Request[v1.GetPlanGraphRequest]) (*connect.Response[v1.GetPlanGraphResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	m := req.Msg
	a, err := h.load(ctx, tx, m.Project, m.Workspace, m.Number)
	if err != nil {
		return nil, err
	}
	g, err := graph.Build(a.plan, a.sources)
	if err != nil {
		return nil, authz.Internal(err)
	}
	g = graph.Filter(g, m.Actions, m.Module)

	var stored []byte
	err = tx.QueryRow(ctx, `SELECT positions FROM graph_layouts WHERE workspace_id = $1`, a.workspaceID).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, authz.Internal(err)
	}
	positions := map[string][2]float64{}
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &positions); err != nil {
			return nil, authz.Internal(err)
		}
	}
	out := &v1.GetPlanGraphResponse{Graph: g, Number: a.number, SourceBaseUrl: a.baseURL}
	keep := func(id string) {
		if p, ok := positions[id]; ok {
			out.Layout = append(out.Layout, &v1.NodePosition{Id: id, X: p[0], Y: p[1]})
		}
	}
	for _, n := range g.Nodes {
		keep(n.Id)
	}
	for _, gr := range g.Groups {
		keep(gr.Id)
	}
	return connect.NewResponse(out), nil
}

func (h Graph) GetPlanNode(ctx context.Context, req *connect.Request[v1.GetPlanNodeRequest]) (*connect.Response[v1.GetPlanNodeResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	m := req.Msg
	a, err := h.load(ctx, tx, m.Project, m.Workspace, m.Number)
	if err != nil {
		return nil, err
	}
	n, err := graph.Node(a.plan, a.sources, m.Id)
	if errors.Is(err, graph.ErrNoNode) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(n), nil
}

func (Graph) SaveGraphLayout(ctx context.Context, req *connect.Request[v1.SaveGraphLayoutRequest]) (*connect.Response[v1.SaveGraphLayoutResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if len(m.Positions) > maxLayoutNodes {
		return nil, invalid(fmt.Sprintf("a layout holds at most %d positions", maxLayoutNodes))
	}
	positions := map[string][2]float64{}
	for _, p := range m.Positions {
		if p.Id == "" || len(p.Id) > maxNodeID || math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) {
			return nil, invalid("each position needs a node id of at most 1024 characters and finite coordinates")
		}
		positions[p.Id] = [2]float64{p.X, p.Y}
	}
	ws, err := workspaceID(ctx, s.Tx, m.Project, m.Workspace)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(positions)
	_, err = s.Tx.Exec(ctx, `INSERT INTO graph_layouts (workspace_id, org_id, positions) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET updated_at = now(),
		positions = CASE WHEN $4 THEN EXCLUDED.positions ELSE graph_layouts.positions || EXCLUDED.positions END`,
		ws, s.OrgID, b, m.Replace)
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("graph layout %s/%s replace=%t", m.Project, m.Workspace, m.Replace))
	return connect.NewResponse(&v1.SaveGraphLayoutResponse{}), nil
}

func (h Graph) GetTopology(ctx context.Context, req *connect.Request[v1.GetTopologyRequest]) (*connect.Response[v1.GetTopologyResponse], error) {
	tx := authz.ScopeFrom(ctx).Tx
	project := req.Msg.Project
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM projects WHERE slug = $1 AND archived_at IS NULL)`, project).Scan(&exists); err != nil {
		return nil, authz.Internal(err)
	}
	if !exists {
		return nil, errNotFound
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (w.id) p.slug, w.name, r.number, j.id
		FROM runs r JOIN jobs j ON j.id = r.plan_job_id JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id
		WHERE w.archived_at IS NULL AND p.archived_at IS NULL AND r.trigger IN ('ui', 'push', 'schedule') AND j.status = 'succeeded'
		ORDER BY w.id, r.number DESC`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type latest struct {
		project, workspace, job string
		number                  int32
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (latest, error) {
		var l latest
		return l, r.Scan(&l.project, &l.workspace, &l.number, &l.job)
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	slices.SortFunc(all, func(a, b latest) int { return strings.Compare(a.project+"/"+a.workspace, b.project+"/"+b.workspace) })
	out := &v1.GetTopologyResponse{}
	var plans []topology.Plan
	var read []latest
	for _, l := range all {
		b, err := h.Jobs.ReadArtifact(ctx, tx, l.job, "plan.json")
		if errors.Is(err, jobs.ErrNoArtifact) {
			continue
		}
		if err != nil {
			return nil, authz.Internal(err)
		}
		plans = append(plans, topology.Plan{Project: l.project, Workspace: l.workspace, JSON: b})
		read = append(read, l)
	}
	full, err := topology.Build(plans)
	if err != nil {
		return nil, authz.Internal(err)
	}
	out.Topology = topology.Scope(full, project)
	drawn := map[[2]string]bool{}
	for _, n := range out.Topology.Nodes {
		drawn[[2]string{n.Project, n.Workspace}] = true
	}
	for _, l := range read {
		if drawn[[2]string{l.project, l.workspace}] {
			out.Sources = append(out.Sources, fmt.Sprintf("%s/%s #%d", l.project, l.workspace, l.number))
		}
	}

	rows, err = tx.Query(ctx, `SELECT p.slug, w.name, f.resource, f.severity FROM findings f JOIN workspaces w ON w.id = f.workspace_id
		JOIN projects p ON p.id = w.project_id WHERE f.fixed_at IS NULL`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type found struct {
		key, severity string
	}
	open, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (found, error) {
		var project, workspace, resource string
		var f found
		err := r.Scan(&project, &workspace, &resource, &f.severity)
		f.key = project + "/" + workspace + "/" + resource
		return f, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	counts, worst := map[string]int32{}, map[string]v1.Severity{}
	for _, f := range open {
		counts[f.key]++
		worst[f.key] = max(worst[f.key], severities[f.severity])
	}
	for _, n := range out.Topology.Nodes {
		if n.Context {
			continue
		}
		k := n.Id
		if counts[k] == 0 {
			k = n.Project + "/" + n.Workspace + "/" + graph.StripKeys(n.Address)
		}
		n.Findings, n.Worst = counts[k], worst[k]
	}
	return connect.NewResponse(out), nil
}

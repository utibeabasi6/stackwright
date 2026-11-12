package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/engine"
)

const sampleEngineVersion = "1.10.7"

var (
	versionPattern   = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
	workspacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,38}[a-z0-9])?$`)
)

type Projects struct {
	stackwrightv1connect.UnimplementedProjectServiceHandler
	Engines *engine.Mirror
}

var engines = map[v1.Engine]string{v1.Engine_ENGINE_OPENTOFU: "opentofu", v1.Engine_ENGINE_TERRAFORM: "terraform"}
var layouts = map[v1.Layout]string{v1.Layout_LAYOUT_PLAIN: "plain", v1.Layout_LAYOUT_CLI_WORKSPACES: "cli_workspaces"}

func key[K comparable](m map[K]string, v string) K {
	for k, s := range m {
		if s == v {
			return k
		}
	}
	var zero K
	return zero
}

func checkEngine(engine v1.Engine, version string) error {
	m := versionPattern.FindStringSubmatch(version)
	if m == nil {
		return invalid("the engine version must look like 1.10.7")
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	if minor > 999 || patch > 999 {
		return invalid("the engine version must look like 1.10.7")
	}
	n := major*1_000_000 + minor*1_000 + patch
	switch engine {
	case v1.Engine_ENGINE_OPENTOFU:
		if n < 1_008_000 {
			return invalid("Stackwright supports OpenTofu 1.8.0 and newer")
		}
	case v1.Engine_ENGINE_TERRAFORM:
		if n < 1_000_000 {
			return invalid("Stackwright supports Terraform 1.0.0 and newer")
		}
	default:
		return invalid("choose OpenTofu or Terraform")
	}
	return nil
}

func cleanPath(path string) (string, error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	bad := len(path) > 200 || strings.ContainsAny(path, "\\\x00")
	for _, seg := range strings.Split(path, "/") {
		bad = bad || seg == ".." || seg == "." || (seg == "" && path != "")
	}
	if bad {
		return "", invalid("the folder must be a path inside the repository, such as network/ or envs/prod")
	}
	return path, nil
}

func checkWorkspace(name string) error {
	if !workspacePattern.MatchString(name) {
		return invalid(fmt.Sprintf("workspace %q needs 1 to 40 lowercase letters, digits, hyphens or underscores", name))
	}
	return nil
}

type projectRow struct {
	id string
	p  *v1.Project
}

func loadProjects(ctx context.Context, tx pgx.Tx, slug string) ([]projectRow, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, p.slug, p.path, p.engine, p.engine_version, p.layout, p.repo_url, p.branch, p.sample, p.created_at,
		coalesce(r.full_name, ''), coalesce(nullif(p.branch, ''), r.default_branch, '')
		FROM projects p LEFT JOIN repos r ON lower(r.full_name) = p.repo
		WHERE p.org_id = app_org_id() AND p.archived_at IS NULL AND ($1 = '' OR p.slug = $1) ORDER BY p.slug`, slug)
	if err != nil {
		return nil, err
	}
	projects, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (projectRow, error) {
		p := &v1.Project{}
		var id, engine, layout string
		var created time.Time
		err := r.Scan(&id, &p.Slug, &p.Path, &engine, &p.EngineVersion, &layout, &p.RepoUrl, &p.Branch, &p.Sample, &created,
			&p.LinkedRepo, &p.RunBranch)
		p.Engine, p.Layout, p.CreatedAt = key(engines, engine), key(layouts, layout), timestamppb.New(created)
		return projectRow{id, p}, err
	})
	if err != nil {
		return nil, err
	}
	byID := map[string]*v1.Project{}
	for _, pr := range projects {
		byID[pr.id] = pr.p
	}
	rows, err = tx.Query(ctx, `SELECT w.project_id, w.name, w.created_at, w.auto_apply, w.required_labels, coalesce(a.name, '')
		FROM workspaces w LEFT JOIN cloud_accounts a ON a.id = w.cloud_account_id
		WHERE w.org_id = app_org_id() AND w.archived_at IS NULL ORDER BY w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var projectID, name string
		var created time.Time
		var autoApply bool
		var labels map[string]string
		var account string
		if err := rows.Scan(&projectID, &name, &created, &autoApply, &labels, &account); err != nil {
			return nil, err
		}
		if p := byID[projectID]; p != nil {
			p.Workspaces = append(p.Workspaces, &v1.Workspace{Name: name, CreatedAt: timestamppb.New(created), AutoApply: autoApply,
				RequiredLabels: labels, CloudAccount: account})
		}
	}
	return projects, rows.Err()
}

func project(ctx context.Context, tx pgx.Tx, slug string) (projectRow, error) {
	ps, err := loadProjects(ctx, tx, slug)
	if err != nil {
		return projectRow{}, authz.Internal(err)
	}
	if slug == "" || len(ps) == 0 {
		return projectRow{}, errNotFound
	}
	return ps[0], nil
}

func (h Projects) ListProjects(ctx context.Context, _ *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	ps, err := loadProjects(ctx, authz.ScopeFrom(ctx).Tx, "")
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := make([]*v1.Project, len(ps))
	for i, pr := range ps {
		out[i] = pr.p
	}
	return connect.NewResponse(&v1.ListProjectsResponse{Projects: out}), nil
}

func (h Projects) GetWorkspace(ctx context.Context, req *connect.Request[v1.GetWorkspaceRequest]) (*connect.Response[v1.GetWorkspaceResponse], error) {
	pr, err := project(ctx, authz.ScopeFrom(ctx).Tx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	for _, w := range pr.p.Workspaces {
		if w.Name == req.Msg.Workspace {
			return connect.NewResponse(&v1.GetWorkspaceResponse{Project: pr.p, Workspace: w}), nil
		}
	}
	return nil, errNotFound
}

func insertProject(ctx context.Context, s *authz.Scope, p *v1.Project, workspaces []string, sample bool) (*v1.Project, error) {
	id := uuid.NewString()
	_, err := s.Tx.Exec(ctx, `INSERT INTO projects (id, org_id, slug, path, engine, engine_version, layout, repo_url, branch, sample)
		VALUES ($1, app_org_id(), $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, p.Slug, p.Path, engines[p.Engine], p.EngineVersion, layouts[p.Layout], p.RepoUrl, p.Branch, sample)
	if uniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("a project named %s exists", p.Slug))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	for _, w := range workspaces {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO workspaces (id, org_id, project_id, name) VALUES ($1, app_org_id(), $2, $3)`,
			uuid.NewString(), id, w); err != nil {
			return nil, authz.Internal(err)
		}
	}
	s.Target("project:" + p.Slug)
	pr, err := project(ctx, s.Tx, p.Slug)
	return pr.p, err
}

func (h Projects) CreateProject(ctx context.Context, req *connect.Request[v1.CreateProjectRequest]) (*connect.Response[v1.CreateProjectResponse], error) {
	m := req.Msg
	if !slugPattern.MatchString(m.Slug) {
		return nil, invalid("the project's short name needs 1 to 40 lowercase letters, digits or hyphens")
	}
	path, err := cleanPath(m.Path)
	if err != nil {
		return nil, err
	}
	if err := checkEngine(m.Engine, m.EngineVersion); err != nil {
		return nil, err
	}
	if len(m.RepoUrl) > 300 || len(m.Branch) > 200 {
		return nil, invalid("the repository URL or branch is too long")
	}
	s := authz.ScopeFrom(ctx)
	var account *v1.CloudAccount
	if m.CloudAccount != "" {
		if account, err = oneAccount(ctx, s.Tx, m.CloudAccount); err != nil {
			return nil, err
		}
	}
	workspaces := m.Workspaces
	switch m.Layout {
	case v1.Layout_LAYOUT_PLAIN:
		workspaces = []string{"default"}
	case v1.Layout_LAYOUT_CLI_WORKSPACES:
		if len(workspaces) == 0 {
			return nil, invalid("name at least one workspace")
		}
		seen := map[string]bool{}
		for _, w := range workspaces {
			if err := checkWorkspace(w); err != nil {
				return nil, err
			}
			if seen[w] {
				return nil, invalid(fmt.Sprintf("workspace %q is listed twice", w))
			}
			seen[w] = true
		}
	default:
		return nil, invalid("choose a plain folder or CLI workspaces")
	}
	p, err := insertProject(ctx, s, &v1.Project{
		Slug: m.Slug, Path: path, Engine: m.Engine, EngineVersion: m.EngineVersion,
		Layout: m.Layout, RepoUrl: strings.TrimSpace(m.RepoUrl), Branch: strings.TrimSpace(m.Branch),
	}, workspaces, false)
	if err != nil {
		return nil, err
	}
	if account != nil {
		if _, err := s.Tx.Exec(ctx, `UPDATE workspaces w SET cloud_account_id = $1 FROM projects p
			WHERE p.id = w.project_id AND p.slug = $2 AND p.archived_at IS NULL`, account.GetId(), m.Slug); err != nil {
			return nil, authz.Internal(err)
		}
	}
	return connect.NewResponse(&v1.CreateProjectResponse{Project: p}), nil
}

func (h Projects) ListEngineVersions(ctx context.Context, req *connect.Request[v1.ListEngineVersionsRequest]) (*connect.Response[v1.ListEngineVersionsResponse], error) {
	name, ok := engines[req.Msg.GetEngine()]
	if !ok {
		return nil, invalid("choose OpenTofu or Terraform")
	}
	all, err := h.Engines.Versions(ctx, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("the %s release list: %w", name, err))
	}
	out := &v1.ListEngineVersionsResponse{}
	for _, v := range all {
		if checkEngine(req.Msg.GetEngine(), v) == nil {
			out.Versions = append(out.Versions, v)
		}
	}
	return connect.NewResponse(out), nil
}

func (h Projects) CreateSampleProject(ctx context.Context, _ *connect.Request[v1.CreateSampleProjectRequest]) (*connect.Response[v1.CreateSampleProjectResponse], error) {
	s := authz.ScopeFrom(ctx)
	var n int
	if err := s.Tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE org_id = app_org_id() AND archived_at IS NULL AND sample`).Scan(&n); err != nil {
		return nil, authz.Internal(err)
	}
	slug := "sample"
	if n > 0 {
		slug = fmt.Sprintf("sample-%d", n+1)
	}
	p, err := insertProject(ctx, s, &v1.Project{
		Slug: slug, Path: "sample", Engine: v1.Engine_ENGINE_OPENTOFU, EngineVersion: sampleEngineVersion,
		Layout: v1.Layout_LAYOUT_CLI_WORKSPACES,
	}, []string{"dev", "prod"}, true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CreateSampleProjectResponse{Project: p}), nil
}

func (h Projects) UpdateProject(ctx context.Context, req *connect.Request[v1.UpdateProjectRequest]) (*connect.Response[v1.UpdateProjectResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	pr, err := project(ctx, s.Tx, m.Project)
	if err != nil {
		return nil, err
	}
	path, err := cleanPath(m.Path)
	if err != nil {
		return nil, err
	}
	if err := checkEngine(pr.p.Engine, m.EngineVersion); err != nil {
		return nil, err
	}
	if len(m.RepoUrl) > 300 || len(m.Branch) > 200 {
		return nil, invalid("the repository URL or branch is too long")
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE projects SET path = $2, engine_version = $3, repo_url = $4, branch = $5 WHERE id = $1`,
		pr.id, path, m.EngineVersion, strings.TrimSpace(m.RepoUrl), strings.TrimSpace(m.Branch)); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("project:" + pr.p.Slug)
	pr, err = project(ctx, s.Tx, m.Project)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.UpdateProjectResponse{Project: pr.p}), nil
}

func refuseLocked(ctx context.Context, tx pgx.Tx, projectID, workspace string) error {
	var name, who string
	var since time.Time
	err := tx.QueryRow(ctx, `SELECT w.name, coalesce(nullif(u.name, ''), u.email, 'someone'), l.created_at
		FROM state_locks l JOIN workspaces w ON w.id = l.workspace_id LEFT JOIN users u ON u.id = l.locked_by
		WHERE w.project_id = $1 AND ($2 = '' OR w.name = $2) LIMIT 1`, projectID, workspace).Scan(&name, &who, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return authz.Internal(err)
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"workspace %s is locked by %s since %s; wait for it to finish, or force-unlock it first", name, who, since.UTC().Format("15:04 MST")))
}

func (h Projects) ArchiveProject(ctx context.Context, req *connect.Request[v1.ArchiveProjectRequest]) (*connect.Response[v1.ArchiveProjectResponse], error) {
	s := authz.ScopeFrom(ctx)
	pr, err := project(ctx, s.Tx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	if err := refuseLocked(ctx, s.Tx, pr.id, ""); err != nil {
		return nil, err
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET archived_at = now() WHERE project_id = $1 AND archived_at IS NULL`, pr.id); err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, pr.id); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("project:" + pr.p.Slug)
	return connect.NewResponse(&v1.ArchiveProjectResponse{}), nil
}

func (h Projects) CreateWorkspace(ctx context.Context, req *connect.Request[v1.CreateWorkspaceRequest]) (*connect.Response[v1.CreateWorkspaceResponse], error) {
	s := authz.ScopeFrom(ctx)
	pr, err := project(ctx, s.Tx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	if pr.p.Layout == v1.Layout_LAYOUT_PLAIN {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a plain folder has one workspace, default"))
	}
	if err := checkWorkspace(req.Msg.Name); err != nil {
		return nil, err
	}
	_, err = s.Tx.Exec(ctx, `INSERT INTO workspaces (id, org_id, project_id, name) VALUES ($1, app_org_id(), $2, $3)`,
		uuid.NewString(), pr.id, req.Msg.Name)
	if uniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already has a workspace named %s", pr.p.Slug, req.Msg.Name))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("workspace:" + pr.p.Slug + "/" + req.Msg.Name)
	return connect.NewResponse(&v1.CreateWorkspaceResponse{Workspace: &v1.Workspace{Name: req.Msg.Name, CreatedAt: timestamppb.Now()}}), nil
}

func (h Projects) ArchiveWorkspace(ctx context.Context, req *connect.Request[v1.ArchiveWorkspaceRequest]) (*connect.Response[v1.ArchiveWorkspaceResponse], error) {
	s := authz.ScopeFrom(ctx)
	pr, err := project(ctx, s.Tx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	found := false
	for _, w := range pr.p.Workspaces {
		found = found || w.Name == req.Msg.Workspace
	}
	if !found {
		return nil, errNotFound
	}
	if len(pr.p.Workspaces) == 1 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this is the project's last workspace; archive the project instead"))
	}
	if err := refuseLocked(ctx, s.Tx, pr.id, req.Msg.Workspace); err != nil {
		return nil, err
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET archived_at = now() WHERE project_id = $1 AND name = $2 AND archived_at IS NULL`,
		pr.id, req.Msg.Workspace); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("workspace:" + pr.p.Slug + "/" + req.Msg.Workspace)
	return connect.NewResponse(&v1.ArchiveWorkspaceResponse{}), nil
}

func (h Projects) UpdateWorkspace(ctx context.Context, req *connect.Request[v1.UpdateWorkspaceRequest]) (*connect.Response[v1.UpdateWorkspaceResponse], error) {
	s := authz.ScopeFrom(ctx)
	ws, err := workspaceID(ctx, s.Tx, req.Msg.Project, req.Msg.Workspace)
	if err != nil {
		return nil, err
	}
	var changes []string
	if req.Msg.AutoApply != nil {
		if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET auto_apply = $2 WHERE id = $1`, ws, req.Msg.GetAutoApply()); err != nil {
			return nil, authz.Internal(err)
		}
		changes = append(changes, fmt.Sprintf("auto_apply=%t", req.Msg.GetAutoApply()))
	}
	if req.Msg.RequiredLabels != nil {
		labels := req.Msg.GetRequiredLabels().GetValues()
		if err := builders.CheckLabels(labels); err != nil {
			return nil, err
		}
		if labels == nil {
			labels = map[string]string{}
		}
		if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET required_labels = $2 WHERE id = $1`, ws, labels); err != nil {
			return nil, authz.Internal(err)
		}
		changes = append(changes, "required_labels="+formatLabels(labels))
	}
	if len(changes) == 0 {
		return nil, invalid("nothing to change")
	}
	s.Target(fmt.Sprintf("workspace:%s/%s %s", req.Msg.Project, req.Msg.Workspace, strings.Join(changes, " ")))
	return connect.NewResponse(&v1.UpdateWorkspaceResponse{}), nil
}

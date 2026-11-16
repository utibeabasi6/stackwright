package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/github"
	"github.com/stackwright/stackwright/internal/importer"
	"github.com/stackwright/stackwright/internal/runs"
	"github.com/stackwright/stackwright/internal/stacks"
)

// Imports imports a linked repository's root modules as projects on the
// repository's own backend (adr/0036).
type Imports struct {
	stackwrightv1connect.UnimplementedImportServiceHandler
	GitHub       *github.GitHub
	Orchestrator *runs.Orchestrator
}

func (h Imports) scan(ctx context.Context, tx pgx.Tx, orgID, repo string) (string, importer.Result, map[string][]byte, error) {
	commit, names, files, err := h.GitHub.RepositoryHead(ctx, tx, orgID, repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", importer.Result{}, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no linked GitHub installation covers %s", repo))
	}
	if err != nil {
		return "", importer.Result{}, nil, githubError(err)
	}
	return commit, importer.Scan(repo, names, files), files, nil
}

// backendOf says where a root's state lives: its backend block, a file in
// the repository for the local backend, or Stackwright for a Stack.
func backendOf(r importer.Root) v1.StateBackend {
	switch {
	case r.IsStack():
		return v1.StateBackend_STATE_BACKEND_STACKWRIGHT
	case r.Backend == "local":
		return v1.StateBackend_STATE_BACKEND_GIT
	}
	return v1.StateBackend_STATE_BACKEND_CODE
}

func existingProject(ctx context.Context, tx pgx.Tx, repo, path string) (string, error) {
	var slug string
	err := tx.QueryRow(ctx, `SELECT slug FROM projects WHERE repo = lower($1) AND path = $2 AND archived_at IS NULL`, repo, path).Scan(&slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return slug, err
}

func (h Imports) ScanRepository(ctx context.Context, req *connect.Request[v1.ScanRepositoryRequest]) (*connect.Response[v1.ScanRepositoryResponse], error) {
	s := authz.ScopeFrom(ctx)
	commit, res, _, err := h.scan(ctx, s.Tx, s.OrgID, req.Msg.Repo)
	if err != nil {
		return nil, err
	}
	out := &v1.ScanRepositoryResponse{Commit: commit, Modules: res.Modules, TerragruntUnits: res.Terragrunt}
	for _, r := range res.Roots {
		existing, err := existingProject(ctx, s.Tx, req.Msg.Repo, r.Path)
		if err != nil {
			return nil, authz.Internal(err)
		}
		out.Roots = append(out.Roots, &v1.FoundRoot{Path: r.Path, Workspaces: r.Workspaces, StateBackend: backendOf(r), Backend: r.Backend,
			Engine: key(engines, r.Engine), Labels: r.Labels, Slug: r.Slug, ExistingProject: existing, CommittedState: r.CommittedState,
			Units: r.Units, StackDeployments: r.StackDeployments, StackError: r.StackError})
	}
	return connect.NewResponse(out), nil
}

// StartImport creates the chosen roots' projects and workspaces, or finds
// those an earlier import made, and plans each workspace that it creates.
// The roots' workspaces and backends come from a scan of the newest
// commit, not from the request.
func (h Imports) StartImport(ctx context.Context, req *connect.Request[v1.StartImportRequest]) (*connect.Response[v1.StartImportResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if len(m.Roots) == 0 {
		return nil, invalid("choose at least one folder to import")
	}
	commit, res, files, err := h.scan(ctx, s.Tx, s.OrgID, m.Repo)
	if err != nil {
		return nil, err
	}
	var repoID string
	if err := s.Tx.QueryRow(ctx, `SELECT id FROM repos WHERE lower(full_name) = lower($1)`, m.Repo).Scan(&repoID); err != nil {
		return nil, authz.Internal(err)
	}
	importID := uuid.NewString()
	if _, err := s.Tx.Exec(ctx, `INSERT INTO imports (id, org_id, repo_id, commit_sha, created_by) VALUES ($1, app_org_id(), $2, $3, $4)`,
		importID, repoID, commit, s.UserID); err != nil {
		return nil, authz.Internal(err)
	}
	var head *runs.NewRun
	for _, chosen := range m.Roots {
		i := slices.IndexFunc(res.Roots, func(r importer.Root) bool { return r.Path == chosen.Path })
		if i < 0 {
			return nil, invalid(fmt.Sprintf("%q is not a root module at %s", chosen.Path, commit[:min(7, len(commit))]))
		}
		root := res.Roots[i]
		if slices.Contains(root.Workspaces, reservedWorkspace) {
			return nil, invalid(fmt.Sprintf("workspace %q is reserved for the project's Topology page, and an import cannot rename a CLI workspace",
				reservedWorkspace))
		}
		slug, err := existingProject(ctx, s.Tx, m.Repo, root.Path)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if slug == "" {
			if err := checkProjectSlug(chosen.Slug); err != nil {
				return nil, err
			}
			if err := checkEngine(chosen.Engine, chosen.EngineVersion); err != nil {
				return nil, err
			}
			if err := checkProjectLabels(chosen.Labels); err != nil {
				return nil, err
			}
			layout := v1.Layout_LAYOUT_PLAIN
			switch {
			case root.IsStack():
				layout = v1.Layout_LAYOUT_STACKS
			case len(root.Units) > 0:
				layout = v1.Layout_LAYOUT_TERRAGRUNT
			case !slices.Equal(root.Workspaces, []string{"default"}):
				layout = v1.Layout_LAYOUT_CLI_WORKSPACES
			}
			if _, err := insertProject(ctx, s, &v1.Project{Slug: chosen.Slug, Path: root.Path, Engine: chosen.Engine,
				EngineVersion: chosen.EngineVersion, Layout: layout, RepoUrl: "https://github.com/" + m.Repo, Labels: chosen.Labels,
				StateBackend: backendOf(root)}, nil, false); err != nil {
				return nil, err
			}
			slug = chosen.Slug
		}
		var accountID *string
		if chosen.CloudAccount != "" {
			account, err := oneAccount(ctx, s.Tx, chosen.CloudAccount)
			if err != nil {
				return nil, err
			}
			id := account.GetId()
			accountID = &id
		}
		if root.IsStack() {
			if err := h.importStack(ctx, s, importID, slug, stacks.FilesIn(files, root.Path), accountID, &head); err != nil {
				return nil, err
			}
			continue
		}
		for _, name := range root.Workspaces {
			var wsID string
			var created bool
			err := s.Tx.QueryRow(ctx, `INSERT INTO workspaces (id, org_id, project_id, name, cloud_account_id)
				SELECT $1, app_org_id(), p.id, $3, $4 FROM projects p WHERE p.slug = $2 AND p.archived_at IS NULL
				ON CONFLICT DO NOTHING RETURNING id`, uuid.NewString(), slug, name, accountID).Scan(&wsID)
			switch {
			case err == nil:
				created = true
			case errors.Is(err, pgx.ErrNoRows):
				if err := s.Tx.QueryRow(ctx, `SELECT w.id FROM workspaces w JOIN projects p ON p.id = w.project_id
					WHERE p.slug = $1 AND p.archived_at IS NULL AND w.name = $2 AND w.archived_at IS NULL`, slug, name).Scan(&wsID); err != nil {
					return nil, authz.Internal(err)
				}
			default:
				return nil, authz.Internal(err)
			}
			var runID *string
			if created {
				if head == nil {
					r, _, err := h.GitHub.BranchHead(ctx, s.Tx, s.OrgID, wsID)
					if err != nil {
						return nil, githubError(err)
					}
					head = &r
				}
				r := *head
				r.WorkspaceID, r.PlanOnly, r.CreatedBy = wsID, true, s.UserID
				id, _, err := h.Orchestrator.Create(ctx, s.Tx, s.OrgID, r)
				if err != nil {
					return nil, authz.Internal(err)
				}
				runID = &id
			}
			if _, err := s.Tx.Exec(ctx, `INSERT INTO import_workspaces (import_id, org_id, workspace_id, created, run_id)
				VALUES ($1, app_org_id(), $2, $3, $4)`, importID, wsID, created, runID); err != nil {
				return nil, authz.Internal(err)
			}
		}
	}
	s.Target(fmt.Sprintf("import %s of %s at %s", importID, m.Repo, commit))
	if head != nil {
		s.AfterCommit(h.Orchestrator.Wake)
	}
	imp, err := loadImport(ctx, s.Tx, importID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StartImportResponse{Import: imp}), nil
}

// importStack syncs a Stack project with its files and starts a run of
// each of its deployments at the import's commit (adr/0043).
func (h Imports) importStack(ctx context.Context, s *authz.Scope, importID, slug string, files map[string][]byte, accountID *string,
	head **runs.NewRun) error {
	var projectID string
	if err := s.Tx.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1 AND archived_at IS NULL`, slug).Scan(&projectID); err != nil {
		return authz.Internal(err)
	}
	deployments, err := runs.SyncStack(ctx, s.Tx, s.OrgID, projectID, files)
	if err != nil {
		return invalid(fmt.Sprintf("project %s: %v", slug, err))
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET cloud_account_id = $2 WHERE project_id = $1 AND archived_at IS NULL
		AND cloud_account_id IS NULL`, projectID, accountID); err != nil {
		return authz.Internal(err)
	}
	var runIDs []string
	for _, d := range deployments {
		var active bool
		if err := s.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM deployment_runs WHERE deployment_id = $1
			AND status IN ('planning', 'needs_approval', 'applying'))`, d.ID).Scan(&active); err != nil {
			return authz.Internal(err)
		}
		if active {
			continue
		}
		var first string
		if err := s.Tx.QueryRow(ctx, `SELECT workspace_id FROM deployment_steps WHERE deployment_id = $1 LIMIT 1`, d.ID).Scan(&first); err != nil {
			return authz.Internal(err)
		}
		if *head == nil {
			r, _, err := h.GitHub.BranchHead(ctx, s.Tx, s.OrgID, first)
			if err != nil {
				return githubError(err)
			}
			*head = &r
		}
		id, _, err := h.Orchestrator.StartDeployment(ctx, s.Tx, s.OrgID, d.ID, s.UserID, d.Destroy, *head)
		if err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, err)
		}
		runIDs = append(runIDs, id)
	}
	_, err = s.Tx.Exec(ctx, `INSERT INTO import_workspaces (import_id, org_id, workspace_id, created, run_id)
		SELECT $1, app_org_id(), si.workspace_id, r.id IS NOT NULL, r.id FROM stack_instances si
		LEFT JOIN runs r ON r.workspace_id = si.workspace_id AND r.deployment_run_id = ANY($3::uuid[])
		WHERE si.project_id = $2 ON CONFLICT DO NOTHING`, importID, projectID, runIDs)
	if err != nil {
		return authz.Internal(err)
	}
	return nil
}

func (h Imports) GetImport(ctx context.Context, req *connect.Request[v1.GetImportRequest]) (*connect.Response[v1.GetImportResponse], error) {
	imp, err := loadImport(ctx, authz.ScopeFrom(ctx).Tx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetImportResponse{Import: imp}), nil
}

func loadImport(ctx context.Context, tx pgx.Tx, id string) (*v1.Import, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, errNotFound
	}
	imp := &v1.Import{Id: id}
	var created time.Time
	err := tx.QueryRow(ctx, `SELECT r.full_name, i.commit_sha, coalesce(u.name, u.email, ''), i.created_at FROM imports i
		JOIN repos r ON r.id = i.repo_id LEFT JOIN users u ON u.id = i.created_by WHERE i.id = $1`, id).
		Scan(&imp.Repo, &imp.Commit, &imp.CreatedBy, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	imp.CreatedAt = timestamppb.New(created)
	rows, err := tx.Query(ctx, `SELECT p.slug, w.name, iw.created, coalesce(iw.run_id::text, '') FROM import_workspaces iw
		JOIN workspaces w ON w.id = iw.workspace_id JOIN projects p ON p.id = w.project_id
		WHERE iw.import_id = $1 ORDER BY p.slug, w.name`, id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	type row struct {
		ws    *v1.ImportedWorkspace
		runID string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{ws: &v1.ImportedWorkspace{}}
		return x, r.Scan(&x.ws.Project, &x.ws.Workspace, &x.ws.Created, &x.runID)
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	for _, x := range list {
		if x.runID != "" {
			run, err := oneRun(ctx, tx, x.runID)
			if err != nil {
				return nil, err
			}
			x.ws.RunNumber, x.ws.RunStatus, x.ws.HasChanges, x.ws.RunError = run.Number, run.Status, run.HasChanges, run.Error
		}
		imp.Workspaces = append(imp.Workspaces, x.ws)
	}
	return imp, nil
}

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/github"
)

type GitHub struct {
	GitHub *github.GitHub
}

func githubError(err error) error {
	switch {
	case errors.Is(err, github.ErrNoApp):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("an instance admin must create the GitHub App first"))
	case errors.Is(err, github.ErrAppExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, github.ErrForbidden), errors.Is(err, github.ErrNotFound), errors.Is(err, github.ErrBadRequest):
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return authz.Internal(err)
}

func (h GitHub) GetGitHub(ctx context.Context, req *connect.Request[v1.GetGitHubRequest]) (*connect.Response[v1.GetGitHubResponse], error) {
	s := authz.ScopeFrom(ctx)
	out := &v1.GetGitHubResponse{}
	app, err := h.GitHub.App(ctx)
	switch {
	case errors.Is(err, github.ErrNoApp):
		return connect.NewResponse(out), nil
	case err != nil:
		return nil, githubError(err)
	}
	out.App = &v1.GitHubApp{Name: app.Name, Slug: app.Slug, HtmlUrl: app.HTMLURL, Owner: app.Owner}
	rows, err := s.Tx.Query(ctx, `SELECT i.installation_id, i.account, coalesce(nullif(u.name, ''), u.email, ''), i.linked_at
		FROM github_installations i LEFT JOIN users u ON u.id = i.linked_by WHERE i.org_id = $1 ORDER BY i.account`, s.OrgID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	installations, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Installation, error) {
		i := &v1.Installation{}
		var at time.Time
		err := r.Scan(&i.InstallationId, &i.Account, &i.LinkedBy, &at)
		i.LinkedAt = timestamppb.New(at)
		return i, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	byID := map[int64]*v1.Installation{}
	for _, i := range installations {
		byID[i.GetInstallationId()] = i
	}
	rows, err = s.Tx.Query(ctx, `SELECT r.installation_id, r.full_name, r.default_branch, r.private,
		coalesce(array_agg(p.slug ORDER BY p.slug) FILTER (WHERE p.id IS NOT NULL), '{}')
		FROM repos r LEFT JOIN projects p ON p.repo = lower(r.full_name) AND p.archived_at IS NULL
		GROUP BY r.id ORDER BY lower(r.full_name)`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	for rows.Next() {
		var installation int64
		repo := &v1.Repo{}
		if err := rows.Scan(&installation, &repo.FullName, &repo.DefaultBranch, &repo.Private, &repo.Projects); err != nil {
			rows.Close()
			return nil, authz.Internal(err)
		}
		if i := byID[installation]; i != nil {
			i.Repos = append(i.Repos, repo)
		}
	}
	rows.Close()
	out.Installations = installations
	return connect.NewResponse(out), nil
}

func (h GitHub) StartAppManifest(ctx context.Context, req *connect.Request[v1.StartAppManifestRequest]) (*connect.Response[v1.StartAppManifestResponse], error) {
	s := authz.ScopeFrom(ctx)
	to := req.Msg.GetReturnTo()
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || len(to) > 200 {
		return nil, invalid("return_to is a path on this site")
	}
	postURL, manifest, err := h.GitHub.Manifest(ctx, s.Tx, s.UserID, req.Msg.GetGithubOrganization(), to)
	if err != nil {
		if errors.Is(err, github.ErrOwnerName) {
			return nil, invalid(err.Error())
		}
		return nil, githubError(err)
	}
	s.Target("GitHub App manifest")
	return connect.NewResponse(&v1.StartAppManifestResponse{Url: postURL, Manifest: manifest}), nil
}

func (h GitHub) DeleteGitHubApp(ctx context.Context, _ *connect.Request[v1.DeleteGitHubAppRequest]) (*connect.Response[v1.DeleteGitHubAppResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := h.GitHub.DeleteApp(ctx, s.Tx); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("GitHub App")
	return connect.NewResponse(&v1.DeleteGitHubAppResponse{}), nil
}

func (h GitHub) StartInstall(ctx context.Context, req *connect.Request[v1.StartInstallRequest]) (*connect.Response[v1.StartInstallResponse], error) {
	s := authz.ScopeFrom(ctx)
	u, err := h.GitHub.InstallURL(ctx, s.Tx, s.UserID, s.OrgID, "/"+req.Msg.GetOrg()+"/settings/github")
	if err != nil {
		return nil, githubError(err)
	}
	s.Target("GitHub install")
	return connect.NewResponse(&v1.StartInstallResponse{Url: u}), nil
}

func (h GitHub) UnlinkInstallation(ctx context.Context, req *connect.Request[v1.UnlinkInstallationRequest]) (*connect.Response[v1.UnlinkInstallationResponse], error) {
	s := authz.ScopeFrom(ctx)
	tag, err := s.Tx.Exec(ctx, `DELETE FROM github_installations WHERE installation_id = $1 AND org_id = $2`, req.Msg.GetInstallationId(), s.OrgID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("installation not found"))
	}
	s.Target(fmt.Sprintf("GitHub installation %d", req.Msg.GetInstallationId()))
	return connect.NewResponse(&v1.UnlinkInstallationResponse{}), nil
}

func (h GitHub) RefreshRepos(ctx context.Context, _ *connect.Request[v1.RefreshReposRequest]) (*connect.Response[v1.RefreshReposResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := h.GitHub.SyncRepos(ctx, s.Tx, s.OrgID); err != nil {
		return nil, githubError(err)
	}
	s.Target("GitHub repos")
	return connect.NewResponse(&v1.RefreshReposResponse{}), nil
}

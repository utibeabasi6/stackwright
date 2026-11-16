package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/auth"
)

var (
	ErrState      = errors.New("this link is spent, expired or for another user; start again")
	ErrLinkedElse = errors.New("another org already linked this installation")
	ErrNotYours   = errors.New("your GitHub account cannot see this installation")
	ErrAppExists  = errors.New("this instance already has a GitHub App")
	ErrSignedOut  = errors.New("sign in to Stackwright, then start again")
	ErrOwnerName  = errors.New("that is not a GitHub organization name")
)

func validOwnerName(s string) bool {
	return s != "" && len(s) <= 39 && !strings.ContainsAny(s, "/?#&= ")
}

const stateTTL = 10 * time.Minute

// newState records a one-time state for a trip to GitHub. tx needs no org
// setting.
func newState(ctx context.Context, tx pgx.Tx, kind, userID, orgID string, installation int64, returnTo string) (string, error) {
	state := auth.RandomToken()
	_, err := tx.Exec(ctx, `INSERT INTO github_states (state_hash, kind, user_id, org_id, installation_id, return_to, expires_at)
		VALUES ($1, $2, $3, nullif($4, '')::uuid, nullif($5, 0), $6, $7)`,
		auth.HashToken(state), kind, userID, orgID, installation, returnTo, time.Now().Add(stateTTL))
	return state, err
}

type spentState struct {
	orgID        string
	installation int64
	returnTo     string
}

// spendState deletes a state of kind that userID made, and returns it. It
// commits at once, so the state is spent even if the work after it fails.
func (g *GitHub) spendState(ctx context.Context, state, kind, userID string) (spentState, error) {
	var s spentState
	var org *string
	var installation *int64
	err := g.Pool.QueryRow(ctx, `DELETE FROM github_states WHERE state_hash = $1 AND kind = $2 AND user_id = $3 AND expires_at > now()
		RETURNING org_id, installation_id, return_to`, auth.HashToken(state), kind, userID).Scan(&org, &installation, &s.returnTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrState
	}
	if err != nil {
		return s, err
	}
	if org != nil {
		s.orgID = *org
	}
	if installation != nil {
		s.installation = *installation
	}
	return s, nil
}

func audit(ctx context.Context, tx pgx.Tx, orgID, userID, action, target string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target) VALUES (nullif($1, '')::uuid, 'user', $2, $3, $4)`,
		orgID, userID, action, target)
	return err
}

// Manifest returns the form that makes the App, for an instance admin.
// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
func (g *GitHub) Manifest(ctx context.Context, tx pgx.Tx, userID, githubOrg, returnTo string) (postURL, manifest string, err error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM github_app)`).Scan(&exists); err != nil {
		return "", "", err
	}
	if exists {
		return "", "", ErrAppExists
	}
	if githubOrg != "" && !validOwnerName(githubOrg) {
		return "", "", fmt.Errorf("%w: %q", ErrOwnerName, githubOrg)
	}
	state, err := newState(ctx, tx, "manifest", userID, "", 0, returnTo)
	if err != nil {
		return "", "", err
	}
	host := g.PublicURL
	if u, err := url.Parse(g.PublicURL); err == nil {
		host = u.Host
	}
	b, err := json.Marshal(map[string]any{
		"name":                     "Stackwright " + host,
		"url":                      g.PublicURL,
		"hook_attributes":          map[string]any{"url": g.PublicURL + "/webhooks/github", "active": true},
		"redirect_url":             g.PublicURL + "/github/manifest",
		"callback_urls":            []string{g.PublicURL + "/github/authorize"},
		"setup_url":                g.PublicURL + "/github/setup",
		"setup_on_update":          true,
		"request_oauth_on_install": false,
		"public":                   false,
		"default_permissions": map[string]string{"contents": "write", "pull_requests": "write", "checks": "write", "metadata": "read",
			"members": "read"},
		"default_events": []string{"push", "pull_request"},
	})
	if err != nil {
		return "", "", err
	}
	postURL = g.WebURL + "/settings/apps/new"
	if githubOrg != "" {
		postURL = g.WebURL + "/organizations/" + url.PathEscape(githubOrg) + "/settings/apps/new"
	}
	return postURL + "?state=" + url.QueryEscape(state), string(b), nil
}

// FinishManifest trades GitHub's code for the new App's secrets. It
// returns the page to go back to.
func (g *GitHub) FinishManifest(ctx context.Context, userID, state, code string) (returnTo string, err error) {
	s, err := g.spendState(ctx, state, "manifest", userID)
	if err != nil {
		return "", err
	}
	returnTo = s.returnTo
	err = pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
		var a struct {
			ID      int64  `json:"id"`
			Slug    string `json:"slug"`
			Name    string `json:"name"`
			HTMLURL string `json:"html_url"`
			Owner   struct {
				Login string `json:"login"`
			} `json:"owner"`
			ClientID      string `json:"client_id"`
			ClientSecret  string `json:"client_secret"`
			WebhookSecret string `json:"webhook_secret"`
			PEM           string `json:"pem"`
		}
		if err := g.call(ctx, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &a); err != nil {
			return fmt.Errorf("make the GitHub App: %w", err)
		}
		if _, err := parseKey([]byte(a.PEM)); err != nil {
			return err
		}
		var sealed [3][]byte
		for i, v := range []string{a.ClientSecret, a.WebhookSecret, a.PEM} {
			var err error
			if sealed[i], err = g.KEK.Seal(ctx, tx, "", []byte(v)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO github_app (app_id, slug, name, html_url, owner, client_id, client_secret, webhook_secret, private_key, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, a.ID, a.Slug, a.Name, a.HTMLURL, a.Owner.Login, a.ClientID,
			sealed[0], sealed[1], sealed[2], userID); err != nil {
			return err
		}
		defer g.forget()
		return audit(ctx, tx, "", userID, "github.app.create", fmt.Sprintf("GitHub App %s (%d)", a.Slug, a.ID))
	})
	return returnTo, err
}

// DeleteApp forgets the App and every link to it. tx needs no org setting.
func (g *GitHub) DeleteApp(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT lock_repo_runs(NULL)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM github_installations`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM github_app`); err != nil {
		return err
	}
	g.forget()
	return nil
}

// InstallURL sends an org admin to install the App.
func (g *GitHub) InstallURL(ctx context.Context, tx pgx.Tx, userID, orgID, returnTo string) (string, error) {
	app, err := g.App(ctx)
	if err != nil {
		return "", err
	}
	state, err := newState(ctx, tx, "install", userID, orgID, 0, returnTo)
	if err != nil {
		return "", err
	}
	return g.WebURL + "/apps/" + url.PathEscape(app.Slug) + "/installations/new?state=" + url.QueryEscape(state), nil
}

// AfterInstall takes GitHub's setup redirect. Anyone can type an
// installation_id into it, so it only sends the user on to authorize the
// App, which proves what they can see.
// https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/best-practices-for-creating-a-github-app#validate-installation-ids
func (g *GitHub) AfterInstall(ctx context.Context, userID, state string, installation int64) (string, error) {
	app, err := g.App(ctx)
	if err != nil {
		return "", err
	}
	var next string
	s, err := g.spendState(ctx, state, "install", userID)
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
		next, err = newState(ctx, tx, "authorize", userID, s.orgID, installation, s.returnTo)
		return err
	})
	if err != nil {
		return "", err
	}
	q := url.Values{"client_id": {app.ClientID}, "state": {next}, "redirect_uri": {g.PublicURL + "/github/authorize"}}
	return g.WebURL + "/login/oauth/authorize?" + q.Encode(), nil
}

// Authorize links the installation to the org once the user's GitHub
// token lists it. It returns the page to go back to, also on an error
// after the state is spent.
func (g *GitHub) Authorize(ctx context.Context, userID, state, code string) (returnTo string, err error) {
	app, err := g.App(ctx)
	if err != nil {
		return "", err
	}
	s, err := g.spendState(ctx, state, "authorize", userID)
	if err != nil {
		return "", err
	}
	returnTo = s.returnTo
	err = pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
		userToken, err := g.userToken(ctx, app, code)
		if err != nil {
			return err
		}
		account, err := g.visibleInstallation(ctx, userToken, s.installation)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, s.orgID); err != nil {
			return err
		}
		var linkedTo string
		err = tx.QueryRow(ctx, `INSERT INTO github_installations (installation_id, org_id, account, linked_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (installation_id) DO UPDATE SET account = excluded.account RETURNING org_id`,
			s.installation, s.orgID, account, userID).Scan(&linkedTo)
		if err != nil {
			return err
		}
		if linkedTo != s.orgID {
			return ErrLinkedElse
		}
		if err := g.syncRepos(ctx, tx, s.orgID, s.installation); err != nil {
			return err
		}
		return audit(ctx, tx, s.orgID, userID, "github.installation.link", fmt.Sprintf("GitHub installation %d (%s)", s.installation, account))
	})
	return returnTo, err
}

func (g *GitHub) userToken(ctx context.Context, app *App, code string) (string, error) {
	resp, err := g.request(ctx, http.MethodPost, g.WebURL+"/login/oauth/access_token", "", map[string]string{
		"client_id": app.ClientID, "client_secret": app.ClientSecret, "code": code,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("authorize the GitHub App: %s", out.Error)
	}
	return out.AccessToken, nil
}

// visibleInstallation returns the installation's account if the user's
// token lists it and the user holds the account: is it, or is an admin of
// its organization. A collaborator on one repository sees the installation
// too, and must not link it (adr/0047).
// https://docs.github.com/en/rest/apps/installations#list-app-installations-accessible-to-the-user-access-token
// https://docs.github.com/en/rest/orgs/members#get-an-organization-membership-for-the-authenticated-user
func (g *GitHub) visibleInstallation(ctx context.Context, userToken string, installation int64) (string, error) {
	for page := 1; page <= 10; page++ {
		var out struct {
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"account"`
			} `json:"installations"`
		}
		if err := g.call(ctx, http.MethodGet, fmt.Sprintf("/user/installations?per_page=100&page=%d", page), userToken, nil, &out); err != nil {
			return "", err
		}
		for _, i := range out.Installations {
			if i.ID != installation {
				continue
			}
			if i.Account.Type == "Organization" {
				var m struct{ State, Role string }
				err := g.call(ctx, http.MethodGet, "/user/memberships/orgs/"+url.PathEscape(i.Account.Login), userToken, nil, &m)
				if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) || err == nil && (m.State != "active" || m.Role != "admin") {
					return "", ErrNotYours
				}
				return i.Account.Login, err
			}
			var u struct{ Login string }
			if err := g.call(ctx, http.MethodGet, "/user", userToken, nil, &u); err != nil {
				return "", err
			}
			if !strings.EqualFold(u.Login, i.Account.Login) {
				return "", ErrNotYours
			}
			return i.Account.Login, nil
		}
		if len(out.Installations) < 100 {
			break
		}
	}
	return "", ErrNotYours
}

// SyncRepos refreshes the org's repos from each of its installations. tx
// needs the org setting.
func (g *GitHub) SyncRepos(ctx context.Context, tx pgx.Tx, orgID string) error {
	rows, err := tx.Query(ctx, `SELECT installation_id FROM github_installations WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := g.syncRepos(ctx, tx, orgID, id); err != nil {
			return err
		}
	}
	return nil
}

func (g *GitHub) syncRepos(ctx context.Context, tx pgx.Tx, orgID string, installation int64) error {
	token, err := g.installationToken(ctx, installation)
	if err != nil {
		return err
	}
	var keep []int64
	for page := 1; page <= 50; page++ {
		var out struct {
			Repositories []struct {
				ID            int64  `json:"id"`
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
				Private       bool   `json:"private"`
			} `json:"repositories"`
		}
		if err := g.call(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), token, nil, &out); err != nil {
			return err
		}
		for _, r := range out.Repositories {
			keep = append(keep, r.ID)
			if _, err := tx.Exec(ctx, `INSERT INTO repos (id, org_id, installation_id, github_id, full_name, default_branch, private)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (org_id, github_id) DO UPDATE SET installation_id = excluded.installation_id, full_name = excluded.full_name,
					default_branch = excluded.default_branch, private = excluded.private`,
				uuid.NewString(), orgID, installation, r.ID, r.FullName, r.DefaultBranch, r.Private); err != nil {
				return err
			}
		}
		if len(out.Repositories) < 100 {
			break
		}
	}
	if _, err := tx.Exec(ctx, `SELECT lock_repo_runs($1, $2)`, installation, keep); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM repos WHERE installation_id = $1 AND NOT github_id = ANY($2)`, installation, keep)
	if err != nil {
		return err
	}
	slog.Info("github repos synced", "installation", installation, "repos", len(keep), "removed", tag.RowsAffected())
	return nil
}

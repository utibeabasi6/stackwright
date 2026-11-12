package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// visit opens path as the browser would after GitHub sends it back, and
// returns the status and where it redirects to.
func (c *client) visit(path string) (int, string) {
	c.w.t.Helper()
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(c.w.srv.URL + path)
	if err != nil {
		c.w.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func stateOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("no state in %q", raw)
	}
	return u.Query().Get("state")
}

// createApp makes the instance's App the way the browser does.
func (w *world) createApp(owner *client) {
	w.t.Helper()
	out := owner.must("GitHubService/StartAppManifest", map[string]any{"returnTo": "/a/settings/github"})
	if code, to := owner.visit("/github/manifest?code=manifest-code&state=" + stateOf(w.t, out["url"].(string))); code != http.StatusSeeOther || to != "/a/settings/github" {
		w.t.Fatalf("manifest callback: %d %s", code, to)
	}
}

// link installs the App on installation 7 and links it to org a, as an
// installer who can see it.
func (w *world) link(owner *client) {
	w.t.Helper()
	w.github.mu.Lock()
	w.github.installations[7] = "acme"
	w.github.visible = []int64{7}
	w.github.repos = []fakeRepo{{ID: 70, FullName: "acme/infra"}}
	w.github.mu.Unlock()
	out := owner.must("GitHubService/StartInstall", map[string]any{"org": "a"})
	code, next := owner.visit("/github/setup?installation_id=7&setup_action=install&state=" + stateOf(w.t, out["url"].(string)))
	if code != http.StatusSeeOther || !strings.HasPrefix(next, w.github.srv.URL+"/login/oauth/authorize") {
		w.t.Fatalf("setup callback: %d %s", code, next)
	}
	if code, to := owner.visit("/github/authorize?code=good-code&state=" + stateOf(w.t, next)); code != http.StatusSeeOther || to != "/a/settings/github" {
		w.t.Fatalf("authorize callback: %d %s", code, to)
	}
}

func TestGitHubInstallNeedsTheInstallersAuthorization(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	admin := w.member(owner, "a", "admin@a.test", "ROLE_ADMIN")
	if _, code := admin.call("GitHubService/StartAppManifest", map[string]any{"returnTo": "/a/settings/github"}); code != "permission_denied" {
		t.Errorf("an org admin who is not an instance admin made the App: %q", code)
	}
	w.createApp(owner)
	if _, code := owner.call("GitHubService/StartAppManifest", map[string]any{"returnTo": "/a"}); code != "already_exists" {
		t.Errorf("a second App: %q", code)
	}
	if app := owner.must("GitHubService/GetGitHub", map[string]any{"org": "a"})["app"].(map[string]any); app["slug"] != "stackwright-test" {
		t.Errorf("app: %v", app)
	}

	w.github.mu.Lock()
	w.github.installations[8] = "someone-else"
	w.github.mu.Unlock()
	out := admin.must("GitHubService/StartInstall", map[string]any{"org": "a"})
	_, next := admin.visit("/github/setup?installation_id=8&setup_action=install&state=" + stateOf(t, out["url"].(string)))
	code, to := admin.visit("/github/authorize?code=good-code&state=" + stateOf(t, next))
	if code != http.StatusSeeOther || !strings.Contains(to, "error=") || !strings.Contains(to, "cannot+see") {
		t.Errorf("an installation the user cannot see: %d %s", code, to)
	}
	if code, _ := admin.visit("/github/authorize?code=good-code&state=" + stateOf(t, next)); code != http.StatusBadRequest {
		t.Errorf("a spent state: %d", code)
	}
	out = admin.must("GitHubService/StartInstall", map[string]any{"org": "a"})
	if code, _ := owner.visit("/github/setup?installation_id=8&setup_action=install&state=" + stateOf(t, out["url"].(string))); code != http.StatusBadRequest {
		t.Errorf("another user's state: %d", code)
	}

	w.link(admin)
	got := admin.must("GitHubService/GetGitHub", map[string]any{"org": "a"})
	if s := asJSON(got); !strings.Contains(s, `"account":"acme"`) || !strings.Contains(s, `"fullName":"acme/infra"`) || strings.Contains(s, "someone-else") {
		t.Errorf("linked: %s", s)
	}
	if code := w.github.deliver(w, "push", "d-bad", map[string]any{}); code != http.StatusAccepted {
		t.Errorf("a signed delivery: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/webhooks/github", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong signature: %d", resp.StatusCode)
	}
}

const (
	shaBase  = "1111111111111111111111111111111111111111"
	shaHead  = "2222222222222222222222222222222222222222"
	shaBreak = "3333333333333333333333333333333333333333"
	shaMerge = "4444444444444444444444444444444444444444"
	shaFork  = "5555555555555555555555555555555555555555"
)

func pullRequest(action, head string, number int, fork bool) map[string]any {
	headRepo := map[string]any{"id": 70, "full_name": "acme/infra"}
	if fork {
		headRepo = map[string]any{"id": 99, "full_name": "stranger/infra"}
	}
	return map[string]any{
		"action": action, "installation": map[string]any{"id": 7},
		"repository": map[string]any{"id": 70, "full_name": "acme/infra"},
		"pull_request": map[string]any{"number": number,
			"head": map[string]any{"sha": head, "ref": "change", "repo": headRepo},
			"base": map[string]any{"sha": shaBase, "ref": "main", "repo": map[string]any{"id": 70, "full_name": "acme/infra"}},
		},
	}
}

func (w *runWorld) runsOf(project, workspace string) []map[string]any {
	out := w.owner.must("RunService/ListRuns", map[string]any{"org": "a", "project": project, "workspace": workspace})
	var list []map[string]any
	for _, r := range asList(out["runs"]) {
		list = append(list, r.(map[string]any))
	}
	return list
}

// eventually polls until ok, or fails after a minute and a half.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never happened", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestGitHubPullRequestsPlanAndMergesApply(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	rw := &runWorld{world: w, owner: owner}
	w.createApp(owner)
	w.link(owner)
	w.startBuilder(4)
	for _, p := range []map[string]any{
		{"slug": "network", "path": "network", "workspaces": []string{"dev", "prod"}},
		{"slug": "app", "path": "app", "workspaces": []string{"default"}},
	} {
		owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": p["slug"], "path": p["path"], "engine": "ENGINE_OPENTOFU",
			"engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES", "workspaces": p["workspaces"], "repoUrl": "https://github.com/acme/infra.git"})
	}
	owner.must("ProjectService/UpdateWorkspace", map[string]any{"org": "a", "project": "network", "workspace": "dev", "autoApply": true})

	good := map[string]string{
		"network/main.tf":    "module \"names\" {\n  source = \"../modules/names\"\n}\n\nresource \"terraform_data\" \"vpc\" {\n  input = module.names.vpc\n}\n",
		"modules/names/x.tf": "output \"vpc\" {\n  value = \"vpc-1\"\n}\n",
		"app/main.tf":        "resource \"terraform_data\" \"app\" {\n  input = \"app\"\n}\n",
	}
	broken := map[string]string{}
	for k, v := range good {
		broken[k] = v
	}
	broken["network/main.tf"] = "resource \"terraform_data\" \"vpc\" {\n  input = \"ok\"\n  nope = \"unknown argument\"\n}\n"
	for sha, files := range map[string]map[string]string{shaBase: good, shaHead: good, shaBreak: broken, shaMerge: good, shaFork: good} {
		w.github.commit(sha, files)
	}
	w.github.diff(shaBase, shaHead, "modules/names/x.tf")
	w.github.diff(shaBase, shaBreak, "network/main.tf")
	w.github.diff(shaBase, shaMerge, "network/main.tf")

	// A pull request that touches only a module of network plans network's
	// two workspaces, once, even when GitHub delivers the event twice.
	for range 2 {
		if code := w.github.deliver(w, "pull_request", "delivery-1", pullRequest("opened", shaHead, 12, false)); code != http.StatusAccepted {
			t.Fatalf("deliver: %d", code)
		}
	}
	for _, ws := range []string{"dev", "prod"} {
		rw.waitFor(map[string]any{"org": "a", "project": "network", "workspace": ws}, 1, "RUN_STATUS_PLANNED")
	}
	if code := w.github.deliver(w, "pull_request", "delivery-2", pullRequest("synchronize", shaHead, 12, false)); code != http.StatusAccepted {
		t.Fatalf("deliver: %d", code)
	}
	time.Sleep(2 * time.Second)
	for _, c := range []struct {
		project, workspace string
		runs               int
	}{{"network", "dev", 1}, {"network", "prod", 1}, {"app", "default", 0}} {
		got := rw.runsOf(c.project, c.workspace)
		if len(got) != c.runs {
			t.Errorf("%s/%s: %d runs, want %d", c.project, c.workspace, len(got), c.runs)
			continue
		}
		if c.runs > 0 && (got[0]["trigger"] != "pull_request" || got[0]["prNumber"] != float64(12) || got[0]["commitSha"] != shaHead ||
			got[0]["planOnly"] != true || got[0]["repo"] != "acme/infra") {
			t.Errorf("%s/%s run: %v", c.project, c.workspace, got[0])
		}
	}
	eventually(t, "two green checks and one comment", func() bool {
		checks := w.github.allChecks()
		w.github.mu.Lock()
		posts := w.github.commentPosts
		w.github.mu.Unlock()
		green := 0
		for _, c := range checks {
			if c.HeadSHA == shaHead && c.Conclusion == "success" && strings.HasPrefix(c.Title, "Planned: +1") {
				green++
			}
		}
		return green == 2 && posts == 1
	})

	// A plan error turns the check red, with an annotation on the line.
	w.github.deliver(w, "pull_request", "delivery-3", pullRequest("synchronize", shaBreak, 12, false))
	eventually(t, "a red check on the failing line", func() bool {
		for _, c := range w.github.allChecks() {
			if c.HeadSHA == shaBreak && c.Conclusion == "failure" && len(c.Annotations) > 0 {
				a := c.Annotations[0]
				return a["path"] == "network/main.tf" && a["start_line"] == float64(3)
			}
		}
		return false
	})
	w.github.mu.Lock()
	posts, comment := w.github.commentPosts, ""
	for _, body := range w.github.comments {
		comment = body
	}
	w.github.mu.Unlock()
	if posts != 1 || !strings.Contains(comment, "`3333333`") || !strings.Contains(comment, "Failed") {
		t.Errorf("the comment after the failing push (%d posts):\n%s", posts, comment)
	}

	// A fork's pull request never plans; its check says why.
	w.github.diff(shaBase, shaFork, "network/main.tf")
	w.github.deliver(w, "pull_request", "delivery-4", pullRequest("opened", shaFork, 13, true))
	eventually(t, "a neutral check for the fork", func() bool {
		return slices.ContainsFunc(w.github.allChecks(), func(c fakeCheck) bool {
			return c.HeadSHA == shaFork && c.Name == "stackwright" && c.Conclusion == "neutral"
		})
	})

	// The merge applies dev, which applies with no approval, and waits for
	// approval on prod.
	w.github.deliver(w, "push", "delivery-5", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaMerge,
		"installation": map[string]any{"id": 7}, "repository": map[string]any{"id": 70, "full_name": "acme/infra"},
	})
	dev := map[string]any{"org": "a", "project": "network", "workspace": "dev"}
	prod := map[string]any{"org": "a", "project": "network", "workspace": "prod"}
	number := func(ws map[string]any) int {
		return int(rw.runsOf(ws["project"].(string), ws["workspace"].(string))[0]["number"].(float64))
	}
	eventually(t, "push runs", func() bool { return len(rw.runsOf("network", "dev")) == 3 && len(rw.runsOf("network", "prod")) == 3 })
	if run := rw.waitFor(dev, number(dev), "RUN_STATUS_APPLIED", "RUN_STATUS_FAILED", "RUN_STATUS_NEEDS_APPROVAL"); run["status"] != "RUN_STATUS_APPLIED" ||
		run["trigger"] != "push" || run["branch"] != "main" {
		t.Errorf("dev after the merge: %v", run)
	}
	if run := rw.waitFor(prod, number(prod), "RUN_STATUS_APPLIED", "RUN_STATUS_FAILED", "RUN_STATUS_NEEDS_APPROVAL"); run["status"] != "RUN_STATUS_NEEDS_APPROVAL" {
		t.Errorf("prod after the merge: %v", run)
	}
	eventually(t, "the merge's checks", func() bool {
		var states []string
		for _, c := range w.github.allChecks() {
			if c.HeadSHA == shaMerge {
				states = append(states, c.Conclusion)
			}
		}
		slices.Sort(states)
		return fmt.Sprint(states) == "[action_required success]"
	})

	var archives int
	if err := w.owner.QueryRow(t.Context(), `SELECT count(*) FROM source_archives`).Scan(&archives); err != nil || archives != 3 {
		t.Errorf("source archives: %d, %v; want one for each commit that ran", archives, err)
	}
}

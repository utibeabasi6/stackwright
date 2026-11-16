// Package upgrade_test upgrades a Compose install from the last release to
// the working tree while a run waits for approval (adr/0046).
package upgrade_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/utibeabasi6/stackwright/internal/auth"
)

const (
	project = "stackwright-upgrade"
	server  = "http://127.0.0.1:18580"
)

// override keeps the test's images, ports and cache apart from a dev stack.
const override = `services:
  stackwright:
    image: stackwright/stackwright:upgrade-%[1]s
    ports: !override ["127.0.0.1:18580:8080"]
    environment:
      STACKWRIGHT_PUBLIC_URL: ` + server + `
      STACKWRIGHT_EXECUTOR_IMAGE: stackwright/executor:upgrade-old
  migrate:
    image: stackwright/stackwright:upgrade-%[1]s
  builder:
    image: stackwright/builder:upgrade-%[1]s
    environment:
      STACKWRIGHT_SANDBOX_CACHE: stackwright-upgrade-builder-cache
  executor:
    image: stackwright/executor:upgrade-%[1]s
  postgres:
    ports: !reset []
  nats:
    ports: !reset []
  rustfs:
    ports: !reset []
volumes:
  builder-cache:
    name: stackwright-upgrade-builder-cache
`

func run(t *testing.T, dir string, stdin io.Reader, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdin = dir, stdin
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

type api struct {
	t    *testing.T
	http *http.Client
}

func (a *api) call(procedure string, body any) map[string]any {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, server+"/stackwright.v1."+procedure, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(server)
	for _, c := range a.http.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			req.Header.Set(auth.CSRFHeader, c.Value)
		}
	}
	resp, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		a.t.Fatalf("%s: %d %v", procedure, resp.StatusCode, out)
	}
	return out
}

func (a *api) waitRun(statuses ...string) map[string]any {
	a.t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for {
		run, _ := a.call("RunService/GetRun", map[string]any{"org": "a", "project": "sample", "workspace": "dev", "number": 1})["run"].(map[string]any)
		for _, s := range statuses {
			if run["status"] == s {
				return run
			}
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("run 1 is %v, want one of %v", run["status"], statuses)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestUpgradeWhileARunWaits(t *testing.T) {
	if os.Getenv("STACKWRIGHT_TEST_UPGRADE") == "" {
		t.Skip("set STACKWRIGHT_TEST_UPGRADE=1; it builds two releases' images")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	from := os.Getenv("STACKWRIGHT_UPGRADE_FROM")
	if from == "" {
		out, err := exec.Command("git", "-C", repo, "describe", "--tags", "--abbrev=0").Output()
		if err != nil {
			t.Skip("no release tag; set STACKWRIGHT_UPGRADE_FROM to a commit")
		}
		from = strings.TrimSpace(string(out))
	}

	old := t.TempDir()
	archive := exec.Command("git", "-C", repo, "archive", from)
	tarball, err := archive.Output()
	if err != nil {
		t.Fatalf("git archive %s: %v", from, err)
	}
	run(t, old, bytes.NewReader(tarball), "tar", "-x")
	files := t.TempDir()
	for _, tag := range []string{"old", "new"} {
		if err := os.WriteFile(filepath.Join(files, tag+".yaml"), fmt.Appendf(nil, override, tag), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	compose := func(dir, tag string, args ...string) string {
		return run(t, dir, nil, "docker", append([]string{"compose", "-p", project, "-f", filepath.Join(dir, "compose.yaml"),
			"-f", filepath.Join(files, tag+".yaml")}, args...)...)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(compose(repo, "new", "logs", "--no-color", "--tail", "80", "stackwright", "builder"))
		}
		compose(repo, "new", "down", "-v", "--remove-orphans")
	})

	compose(old, "old", "up", "-d", "--build", "--wait")
	setup := regexp.MustCompile(`"setup_code":"([a-z0-9]+)"`).FindStringSubmatch(compose(old, "old", "logs", "--no-color", "stackwright"))
	if setup == nil {
		t.Fatal("no setup code in the old server's log")
	}
	jar, _ := cookiejar.New(nil)
	a := &api{t: t, http: &http.Client{Jar: jar}}
	a.call("AuthService/CompleteSetup", map[string]any{"setupCode": setup[1], "orgName": "Acme", "orgSlug": "a",
		"email": "owner@a.test", "name": "Owner", "password": "correct horse battery"})
	a.call("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	a.call("RunService/CreateRun", map[string]any{"org": "a", "project": "sample", "workspace": "dev"})
	a.waitRun("RUN_STATUS_NEEDS_APPROVAL")
	builder := strings.TrimSpace(compose(old, "old", "ps", "-q", "builder"))

	compose(repo, "new", "build", "stackwright")
	compose(repo, "new", "run", "--rm", "migrate")
	compose(repo, "new", "up", "-d", "--no-deps", "--wait", "stackwright")
	if got := strings.TrimSpace(compose(repo, "new", "ps", "-q", "builder")); got != builder {
		t.Fatalf("the upgrade replaced the builder: %s, was %s", got, builder)
	}

	a.call("RunService/ApproveRun", map[string]any{"org": "a", "project": "sample", "workspace": "dev", "number": 1})
	if run := a.waitRun("RUN_STATUS_APPLIED", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_APPLIED" {
		t.Fatalf("after the upgrade: %v", run)
	}
}

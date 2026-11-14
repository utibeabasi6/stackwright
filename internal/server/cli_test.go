package server_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/minio/minio-go/v7"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

// cliBinary stands in for the CLI that the image serves.
const cliBinary = "#!/bin/sh\necho stackwright\n"

func cliDir(t *testing.T) string {
	dir := t.TempDir()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(cliBinary))
	_ = zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "stackwright-linux-amd64.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("abc  stackwright-linux-amd64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCLIDownloadsUnpackForClientsWithoutGzip(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	get := func(path, encoding string) (int, http.Header, string) {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
		req.Header.Set("Accept-Encoding", encoding)
		resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	if status, h, body := get("/cli/stackwright-linux-amd64", "identity"); status != http.StatusOK || body != cliBinary || h.Get("Content-Encoding") != "" {
		t.Errorf("without gzip: %d %q %v", status, body, h)
	}
	if _, h, body := get("/cli/stackwright-linux-amd64", "gzip"); h.Get("Content-Encoding") != "gzip" || !strings.HasPrefix(body, "\x1f\x8b") {
		t.Errorf("with gzip: %v", h)
	}
	if _, _, body := get("/cli/SHA256SUMS", ""); !strings.Contains(body, "stackwright-linux-amd64") {
		t.Errorf("sums: %q", body)
	}
	for _, path := range []string{"/cli/stackwright-windows-amd64", "/cli/..%2fSHA256SUMS", "/cli/stackwright-darwin-arm64"} {
		if status, _, _ := get(path, ""); status != http.StatusNotFound {
			t.Errorf("%s: %d", path, status)
		}
	}
}

func postDevice(t *testing.T, w *world, path string, form url.Values) map[string]any {
	t.Helper()
	resp, err := http.PostForm(w.srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCLISignsInPlansTheWorkingTreeAndNeverApplies(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	w.createApp(owner)
	w.link(owner)
	w.startBuilder(2)
	owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "network", "path": "network", "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_CLI_WORKSPACES", "workspaces": []string{"dev", "prod"}, "repoUrl": "https://github.com/acme/infra"})

	bin := filepath.Join(t.TempDir(), "stackwright")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/stackwright").CombinedOutput(); err != nil {
		t.Fatalf("build the CLI: %v %s", err, out)
	}
	repo := t.TempDir()
	for name, body := range map[string]string{
		"network/main.tf":       "module \"names\" {\n  source = \"../modules/names\"\n}\n\nresource \"terraform_data\" \"vpc\" {\n  input = module.names.vpc\n}\n",
		"modules/names/main.tf": "output \"vpc\" {\n  value = \"vpc-1\"\n}\n",
		".gitignore":            "*.secret\n",
		"network/local.secret":  "not for the server\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/infra.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
	cli := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env, cmd.Dir = env, filepath.Join(repo, "network")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// A poll before the interval ends is told to slow down.
	probe := postDevice(t, w, "/cli/device/code", url.Values{"name": {"probe"}})
	poll := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {probe["device_code"].(string)}}
	if got := postDevice(t, w, "/cli/device/token", poll)["error"]; got != "authorization_pending" {
		t.Errorf("first poll: %v", got)
	}
	if got := postDevice(t, w, "/cli/device/token", poll)["error"]; got != "slow_down" {
		t.Errorf("second poll at once: %v", got)
	}

	login := exec.Command(bin, "login", "-server", w.srv.URL, "-name", "laptop")
	login.Env = env
	stdout, err := login.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := login.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	code := ""
	for code == "" && lines.Scan() {
		if m := regexp.MustCompile(`code ([B-Z]{4}-[B-Z]{4})`).FindStringSubmatch(lines.Text()); m != nil {
			code = m[1]
		}
	}
	if dev := owner.must("AuthService/GetDevice", map[string]any{"userCode": strings.ToLower(code)}); dev["name"] != "laptop" {
		t.Errorf("the device: %v", dev)
	}
	owner.must("AuthService/ApproveDevice", map[string]any{"userCode": code})
	var rest strings.Builder
	for lines.Scan() {
		rest.WriteString(lines.Text() + "\n")
	}
	if err := login.Wait(); err != nil || !strings.Contains(rest.String(), "Signed in to "+w.srv.URL+" as owner@") {
		t.Fatalf("login: %v %s", err, rest.String())
	}
	if _, code := owner.call("AuthService/ApproveDevice", map[string]any{"userCode": code}); code != "not_found" {
		t.Errorf("a code approved twice: %q", code)
	}

	if out, err := cli("plan"); err == nil || !strings.Contains(out, "several workspaces: dev, prod; name one with -workspace") {
		t.Errorf("plan with no workspace and no terminal: %v %s", err, out)
	}
	out, err := cli("plan", "-workspace", "dev")
	if err != nil || !strings.HasPrefix(out, "Plan #1 of a/network/dev: ") {
		t.Fatalf("plan: %v\n%s", err, out)
	}

	// The CLI prints the lines the web app gets for the same run.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := owner.runs().WatchRun(ctx, connect.NewRequest(&v1.WatchRunRequest{Org: "a", Project: "network", Workspace: "dev", Number: 1}))
	if err != nil {
		t.Fatal(err)
	}
	var web []string
	for stream.Receive() {
		for _, e := range stream.Msg().GetEvents() {
			line := e.GetText()
			if strings.TrimSpace(line) == "" {
				continue
			}
			if e.GetResourceAddr() != "" && !strings.Contains(line, e.GetResourceAddr()) {
				line += " " + e.GetResourceAddr()
			}
			if e.GetDetail() != "" {
				line += "\n" + e.GetDetail()
			}
			web = append(web, line)
		}
	}
	// Each entry is a line with its time, then the lines indented under it.
	var printed []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n")[2:] {
		if rest, ok := strings.CutPrefix(l, "          "); ok && len(printed) > 0 {
			printed[len(printed)-1] += "\n" + rest
		} else if len(l) > 10 && l[2] == ':' && l[5] == ':' {
			printed = append(printed, l[10:])
		}
	}
	if len(web) < 5 || !slices.Equal(printed, web) {
		t.Errorf("the CLI printed\n%s\nthe web app got\n%s", strings.Join(printed, "\n"), strings.Join(web, "\n"))
	}
	if !slices.Contains(web, "Plan: +1 ~0 −0") || !slices.Contains(web, "✓ Planned") {
		t.Errorf("the timeline: %v", web)
	}

	// The plan is speculative: it planned the upload and nothing applies it.
	run := owner.must("RunService/GetRun", map[string]any{"org": "a", "project": "network", "workspace": "dev", "number": 1})["run"].(map[string]any)
	if run["trigger"] != "cli" || run["planOnly"] != true || run["status"] != "RUN_STATUS_PLANNED" || run["commitSha"] != nil {
		t.Errorf("the run: %v", run)
	}
	var orgID, runID string
	if err := w.owner.QueryRow(context.Background(), `SELECT org_id, id FROM runs WHERE trigger = 'cli'`).Scan(&orgID, &runID); err != nil {
		t.Fatal(err)
	}
	obj, err := w.s3.GetObject(context.Background(), w.bucket, "orgs/"+orgID+"/runs/"+runID+"/source.tar.gz", minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := io.ReadAll(obj)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := w.runs.KEK.Open(context.Background(), tx, orgID, sealed)
	_ = tx.Rollback(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	var uploaded []string
	for tr := tar.NewReader(zr); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		uploaded = append(uploaded, h.Name)
	}
	slices.Sort(uploaded)
	if !slices.Equal(uploaded, []string{".gitignore", "modules/names/main.tf", "network/main.tf"}) {
		t.Errorf("the upload holds %v; the ignored file must stay home", uploaded)
	}
	if _, code := owner.call("RunService/ApproveRun", map[string]any{"org": "a", "project": "network", "workspace": "dev", "number": 1}); code != "failed_precondition" {
		t.Errorf("approve a CLI plan: %q", code)
	}
	if _, err := w.owner.Exec(context.Background(), `UPDATE runs SET plan_only = false WHERE trigger = 'cli'`); err == nil ||
		!strings.Contains(err.Error(), "runs_cli_plan_only") {
		t.Errorf("the database let a CLI run lose plan_only: %v", err)
	}

	if out, err := cli("runs", "-workspace", "dev"); err != nil || !strings.Contains(out, "planned") || !strings.Contains(out, "cli on ") {
		t.Errorf("runs: %v\n%s", err, out)
	}
	if out, err := cli("logs", "-workspace", "dev", "1"); err != nil || !strings.Contains(out, "✓ Planned") {
		t.Errorf("logs: %v\n%s", err, out)
	}

	tokens := asList(owner.must("AuthService/ListCliTokens", map[string]any{})["tokens"])
	if len(tokens) != 1 || tokens[0].(map[string]any)["name"] != "laptop" {
		t.Fatalf("tokens: %v", tokens)
	}
	owner.must("AuthService/RevokeCliToken", map[string]any{"id": tokens[0].(map[string]any)["id"]})
	if out, err := cli("runs", "-workspace", "dev"); err == nil || !strings.Contains(out, "run stackwright login") {
		t.Errorf("runs after a revoke: %v %s", err, out)
	}
}

package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/sandbox"
)

const executorImage = "stackwright/executor:test"

type jobWorld struct {
	*world
	api       *client
	docker    *sandbox.Docker
	platform  string
	orgID     string
	workspace string
}

// newJobWorld serves a world that sandboxes reach at host.docker.internal,
// with an OpenTofu project "net", a Terraform project "legacy", and an
// OpenTofu project "lab" in the folder "live".
// requireDocker builds the executor image, once for each test that needs
// it; Docker's cache makes the later builds quick.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to run jobs in Docker")
	}
	if out, err := exec.Command("docker", "build", "-q", "--target", "executor", "-t", executorImage, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build the executor image: %v: %s", err, out)
	}
}

func newJobWorld(t *testing.T) *jobWorld {
	t.Helper()
	requireDocker(t)
	w := start(t, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.jobs.RunReaper(ctx, time.Second)

	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "legacy", "engine": "ENGINE_TERRAFORM",
		"engineVersion": "1.5.7", "layout": "LAYOUT_PLAIN"})
	owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "lab", "path": "live", "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"})

	d, err := sandbox.NewDocker(sandbox.DockerConfig{ExtraHosts: []string{"host.docker.internal:host-gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := d.Platform(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jw := &jobWorld{world: w, api: owner, docker: d, platform: platform}
	jw.orgID, jw.workspace = jw.workspaceID("net", "prod")
	return jw
}

func (w *jobWorld) workspaceID(project, workspace string) (orgID, id string) {
	w.t.Helper()
	if err := w.owner.QueryRow(context.Background(), `SELECT w.org_id, w.id FROM workspaces w JOIN projects p ON p.id = w.project_id
		WHERE p.slug = $1 AND w.name = $2`, project, workspace).Scan(&orgID, &id); err != nil {
		w.t.Fatal(err)
	}
	return orgID, id
}

func source(t *testing.T, fixture string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := filepath.Join("testdata", "jobs", fixture)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func (w *jobWorld) create(r jobs.Request) (string, string) {
	w.t.Helper()
	if r.OrgID == "" {
		r.OrgID, r.WorkspaceID = w.orgID, w.workspace
	}
	r.Platform = w.platform
	c, err := w.jobs.Create(context.Background(), r)
	if err != nil {
		w.t.Fatal(err)
	}
	return c.ID, c.Ticket
}

func (w *jobWorld) start(ticket string) sandbox.Sandbox {
	w.t.Helper()
	s, err := w.docker.Start(context.Background(), sandbox.Spec{
		Image: executorImage,
		Env:   map[string]string{"STACKWRIGHT_SERVER_URL": w.public},
		Files: map[string][]byte{"ticket": []byte(ticket)},
		Allow: []string{strings.TrimPrefix(w.public, "http://")},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = s.Remove(context.Background()) })
	return s
}

func (w *jobWorld) run(r jobs.Request) (jobID string, code int32, logs string) {
	w.t.Helper()
	id, ticket := w.create(r)
	s := w.start(ticket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	code, err := s.Wait(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	b, _ := s.Logs(ctx)
	return id, code, string(b)
}

type jobEvent struct {
	Type    string
	Addr    string
	Payload map[string]any
}

func (w *jobWorld) events(jobID string) []jobEvent {
	w.t.Helper()
	rows, err := w.owner.Query(context.Background(), `SELECT type, resource_addr, payload FROM job_events WHERE job_id = $1 ORDER BY seq`, jobID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []jobEvent
	for rows.Next() {
		var e jobEvent
		if err := rows.Scan(&e.Type, &e.Addr, &e.Payload); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (w *jobWorld) status(jobID string) (status, msg string, hasChanges *bool) {
	w.t.Helper()
	var e *string
	if err := w.owner.QueryRow(context.Background(), `SELECT status, error, has_changes FROM jobs WHERE id = $1`, jobID).Scan(&status, &e, &hasChanges); err != nil {
		w.t.Fatal(err)
	}
	if e != nil {
		msg = *e
	}
	return status, msg, hasChanges
}

func (w *jobWorld) artifact(jobID, name string) []byte {
	w.t.Helper()
	ctx := context.Background()
	tx, err := w.owner.Begin(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := w.jobs.ReadArtifact(ctx, tx, jobID, name)
	if err != nil {
		w.t.Fatalf("artifact %s: %v", name, err)
	}
	return b
}

func TestJobPlanAndApply(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	planID, code, logs := w.run(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
	if code != 0 {
		t.Fatalf("plan exited %d:\n%s", code, logs)
	}
	if status, msg, changes := w.status(planID); status != "succeeded" || changes == nil || !*changes {
		t.Fatalf("plan: %s %q changes=%v", status, msg, changes)
	}

	planned := map[string]string{}
	for _, e := range w.events(planID) {
		if e.Type == "engine.planned_change" {
			planned[e.Addr] = e.Payload["change"].(map[string]any)["action"].(string)
		}
	}
	var show struct {
		ResourceChanges []struct {
			Address string
			Change  struct{ Actions []string }
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(w.artifact(planID, "plan.json"), &show); err != nil {
		t.Fatal(err)
	}
	fromShow := map[string]string{}
	for _, rc := range show.ResourceChanges {
		if !slices.Equal(rc.Change.Actions, []string{"no-op"}) {
			fromShow[rc.Address] = strings.Join(rc.Change.Actions, ",")
		}
	}
	if len(planned) != 2 || len(fromShow) != len(planned) {
		t.Fatalf("planned_change events %v, show -json %v", planned, fromShow)
	}
	for addr, action := range planned {
		if fromShow[addr] != action {
			t.Errorf("%s: event says %s, show -json says %s", addr, action, fromShow[addr])
		}
	}
	for _, name := range []string{"plan.bin", "plan.json"} {
		if len(w.artifact(planID, name)) == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	applyID, code, logs := w.run(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: planID})
	if code != 0 {
		t.Fatalf("apply exited %d:\n%s", code, logs)
	}
	if status, msg, _ := w.status(applyID); status != "succeeded" {
		t.Fatalf("apply: %s %q", status, msg)
	}
	var order []string
	for _, e := range w.events(applyID) {
		switch e.Type {
		case "engine.apply_start", "engine.apply_complete":
			order = append(order, strings.TrimPrefix(e.Type, "engine.")+" "+e.Addr)
		case "engine.outputs":
			b, _ := json.Marshal(e.Payload)
			if strings.Contains(string(b), "do-not-show") {
				t.Errorf("a sensitive output reached an event: %s", b)
			}
		}
	}
	want := []string{
		"apply_start terraform_data.greeting", "apply_complete terraform_data.greeting",
		"apply_start terraform_data.echo", "apply_complete terraform_data.echo",
	}
	if !slices.Equal(order, want) {
		t.Errorf("apply events:\n got %q\nwant %q", order, want)
	}
	var resources int
	if err := w.owner.QueryRow(context.Background(), `SELECT resources FROM state_versions WHERE workspace_id = $1 ORDER BY serial DESC LIMIT 1`,
		w.workspace).Scan(&resources); err != nil || resources != 2 {
		t.Errorf("state after apply: %d resources, %v", resources, err)
	}
	var actor string
	if err := w.owner.QueryRow(context.Background(), `SELECT actor_kind || ':' || actor_id FROM audit_log WHERE action = 'state.write'`).Scan(&actor); err != nil || actor != "job:"+applyID {
		t.Errorf("state write audited as %q, %v", actor, err)
	}

	if _, err := w.jobs.Create(context.Background(), jobs.Request{OrgID: w.orgID, WorkspaceID: w.workspace,
		Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: applyID}); err == nil {
		t.Error("an apply of an apply was accepted")
	}
}

func TestJobAppliesAPlanMadeAtAnotherAddress(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	planOnly, err := sandbox.NewDocker(sandbox.DockerConfig{ExtraHosts: []string{"plan-only.internal:host-gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	for project, workspace := range map[string]string{"net": "prod", "legacy": "default"} {
		t.Run(project, func(t *testing.T) {
			orgID, ws := w.workspaceID(project, workspace)
			planID, ticket := w.create(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
			other := strings.Replace(w.public, "host.docker.internal", "plan-only.internal", 1)
			// The presigned URLs still name the server's usual address.
			s, err := planOnly.Start(context.Background(), sandbox.Spec{
				Image: executorImage,
				Env:   map[string]string{"STACKWRIGHT_SERVER_URL": other},
				Files: map[string][]byte{"ticket": []byte(ticket)},
				Allow: []string{strings.TrimPrefix(other, "http://"), strings.TrimPrefix(w.public, "http://")},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Remove(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if code, err := s.Wait(ctx); err != nil || code != 0 {
				b, _ := s.Logs(ctx)
				t.Fatalf("plan exited %d, %v:\n%s", code, err, b)
			}

			applyID, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: planID})
			if status, msg, _ := w.status(applyID); code != 0 || status != "succeeded" {
				t.Fatalf("apply exited %d, %s %q:\n%s", code, status, msg, logs)
			}
		})
	}
}

func TestJobApplySeesFilesThePlanWrote(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	orgID, ws := w.workspaceID("lab", "default")
	planID, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "planfiles")})
	if code != 0 {
		t.Fatalf("plan exited %d:\n%s", code, logs)
	}
	applyID, code, _ := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: planID})
	if status, msg, _ := w.status(applyID); code != 0 || status != "succeeded" {
		t.Fatalf("apply exited %d, %s %q: %v", code, status, msg, w.events(applyID))
	}
}

func TestJobTerraform(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	orgID, ws := w.workspaceID("legacy", "default")
	id, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, logs)
	}
	n := 0
	for _, e := range w.events(id) {
		if e.Type == "engine.planned_change" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d planned_change events from Terraform 1.5.7", n)
	}
}

func TestJobDiagnostics(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	id, code, _ := w.run(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "broken")})
	if code == 0 {
		t.Fatal("a broken plan exited 0")
	}
	if status, msg, _ := w.status(id); status != "failed" || !strings.Contains(msg, "plan") {
		t.Errorf("job: %s %q", status, msg)
	}
	found := false
	for _, e := range w.events(id) {
		if e.Type != "engine.diagnostic" {
			continue
		}
		d := e.Payload["diagnostic"].(map[string]any)
		r, ok := d["range"].(map[string]any)
		if !ok {
			continue
		}
		start := r["start"].(map[string]any)
		if d["severity"] == "error" && r["filename"] == "main.tf" && start["line"] == float64(6) {
			found = true
		}
	}
	if !found {
		t.Errorf("no error diagnostic at main.tf:6 in %+v", w.events(id))
	}
}

func TestJobRefusesAChangedEngine(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	w.create(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
	key := "engines/opentofu/1.10.7/" + w.platform + "/tofu"
	evil := []byte("#!/bin/sh\necho changed\n")
	if _, err := w.s3.PutObject(context.Background(), w.bucket, key, bytes.NewReader(evil), int64(len(evil)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	id, code, _ := w.run(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
	if code == 0 {
		t.Fatal("the job ran a changed engine")
	}
	if status, msg, _ := w.status(id); status != "failed" || !strings.Contains(msg, "engine binary does not match") {
		t.Errorf("job: %s %q", status, msg)
	}
	for _, e := range w.events(id) {
		if strings.HasPrefix(e.Type, "engine.") {
			t.Errorf("the engine ran: %s", e.Type)
		}
	}
}

func TestJobLostSandboxReleasesLock(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	planID, code, logs := w.run(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "slow")})
	if code != 0 {
		t.Fatalf("plan exited %d:\n%s", code, logs)
	}
	applyID, ticket := w.create(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: planID})
	s := w.start(ticket)

	deadline := time.Now().Add(3 * time.Minute)
	for !slices.ContainsFunc(w.events(applyID), func(e jobEvent) bool { return e.Type == "engine.apply_start" }) {
		if time.Now().After(deadline) {
			b, _ := s.Logs(context.Background())
			t.Fatalf("the apply never started:\n%s", b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	var locks int
	_ = w.owner.QueryRow(context.Background(), `SELECT count(*) FROM state_locks WHERE job_id = $1`, applyID).Scan(&locks)
	if locks != 1 {
		t.Fatalf("the apply holds %d locks", locks)
	}

	inspect, err := exec.Command("docker", "inspect", s.ID()).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	top, err := exec.Command("docker", "top", s.ID(), "-o", "pid,args").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{"docker inspect": inspect, "docker top": top} {
		if bytes.Contains(out, []byte(ticket)) || bytes.Contains(out, []byte("swj_")) {
			t.Errorf("%s shows a secret:\n%s", name, out)
		}
	}

	killed := time.Now()
	if err := s.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	for {
		_ = w.owner.QueryRow(context.Background(), `SELECT count(*) FROM state_locks WHERE workspace_id = $1`, w.workspace).Scan(&locks)
		if locks == 0 {
			break
		}
		if time.Since(killed) > 60*time.Second {
			t.Fatal("the lock was still held 60 s after the sandbox died")
		}
		time.Sleep(time.Second)
	}
	t.Logf("lock released %s after the kill", time.Since(killed).Round(time.Second))
	if status, msg, _ := w.status(applyID); status != "lost" || msg != "the sandbox stopped reporting" {
		t.Errorf("job: %s %q", status, msg)
	}
}

func TestJobTicketWorksOnce(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	id, ticket := w.create(jobs.Request{Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "ok")})
	call := func(procedure, bearer string, body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/stackwright.v1.ExecutorService/"+procedure, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, claimed := call("ClaimJob", ticket, "{}")
	if code != http.StatusOK {
		t.Fatalf("first claim: %d %v", code, claimed)
	}
	if code, _ := call("ClaimJob", ticket, "{}"); code != http.StatusUnauthorized {
		t.Errorf("second claim: %d", code)
	}
	if status, _, _ := w.status(id); status != "running" {
		t.Errorf("job: %s", status)
	}

	token := claimed["job"].(map[string]any)["token"].(string)
	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", token, ""); code != http.StatusNoContent {
		t.Errorf("the job's own workspace: %d", code)
	}
	if code, _ := w.backend(http.MethodGet, "/state/a/legacy/default", token, ""); code != http.StatusNotFound {
		t.Errorf("another workspace: %d", code)
	}
	if code, out := w.backend(http.MethodPost, "/state/a/net/prod", token, stateDoc(1, "L1", "a")); code != http.StatusForbidden {
		t.Errorf("a plan job wrote state: %d %s", code, out)
	}
	nul := base64.StdEncoding.EncodeToString([]byte(`{"@message":"a\u0000b"}`))
	if code, out := call("ReportEvents", token, `{"events":[{"seq":1,"type":"engine.log","payload":"`+nul+`"}]}`); code != http.StatusOK {
		t.Errorf("an event with a NUL: %d %v", code, out)
	}
	if code, out := call("FinishJob", token, `{"succeeded": true}`); code != http.StatusOK {
		t.Fatalf("finish: %d %v", code, out)
	}
	if code, _ := w.backend(http.MethodGet, "/state/a/net/prod", token, ""); code != http.StatusUnauthorized {
		t.Errorf("after the job ended: %d", code)
	}
	if code, _ := call("ReportEvents", token, "{}"); code != http.StatusUnauthorized {
		t.Errorf("events after the job ended: %d", code)
	}
}

func TestJobDownloadsAGitModule(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	owner := w.client()
	owner.must("AuthService/SignIn", map[string]any{"email": "owner@a.test", "password": password})
	owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "modern", "engine": "ENGINE_TERRAFORM",
		"engineVersion": "1.17.2", "layout": "LAYOUT_PLAIN"})
	for _, project := range [][2]string{{"net", "prod"}, {"legacy", "default"}, {"modern", "default"}} {
		orgID, ws := w.workspaceID(project[0], project[1])
		id, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: source(t, "gitmodule")})
		if code != 0 {
			t.Fatalf("%s: exited %d:\n%s", project[0], code, logs)
		}
		if status, msg, changes := w.status(id); status != "succeeded" || changes == nil || !*changes {
			t.Errorf("%s: job %s %q %v", project[0], status, msg, changes)
		}
		for _, e := range w.events(id) {
			if b, _ := json.Marshal(e.Payload); strings.Contains(string(b), `\u001b[`) {
				t.Errorf("%s: an event keeps a color code: %s", project[0], b)
			}
		}
	}
}

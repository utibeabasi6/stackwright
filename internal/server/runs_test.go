package server_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/builder"
	"github.com/stackwright/stackwright/internal/sandbox"
)

// startBuilder starts the built-in builder in the test process.
func (w *world) startBuilder(slots int32) {
	w.t.Helper()
	id, err := builder.Enrol(context.Background(), builder.PlainClient(w.srv.URL), builderToken, slots, nil, "", w.builderLocal)
	if err != nil {
		w.t.Fatal(err)
	}
	w.runBuilder(id)
}

// runBuilder runs a builder with id until the test ends, and waits until
// it is connected.
func (w *world) runBuilder(id *builder.Identity) {
	w.t.Helper()
	d, err := sandbox.NewDocker(sandbox.DockerConfig{ExtraHosts: []string{"host.docker.internal:host-gateway"}})
	if err != nil {
		w.t.Fatal(err)
	}
	b := &builder.Builder{Identity: id, Backend: d, BackendName: "docker"}
	ctx, cancel := context.WithCancel(context.Background())
	w.t.Cleanup(cancel)
	go b.Run(ctx)
	wait, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := w.hub.WaitOnline(wait, id.ServerID); err != nil {
		w.t.Fatalf("builder %s: %v", id.Name, err)
	}
}

// runWorld has the sample project, a reviewer and a viewer, and a builder.
type runWorld struct {
	*world
	owner, reviewer, viewer *client
}

func newRunWorld(t *testing.T) *runWorld {
	t.Helper()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	owner.must("ProjectService/CreateSampleProject", map[string]any{"org": "a"})
	rw := &runWorld{world: w, owner: owner,
		reviewer: w.member(owner, "a", "reviewer@a.test", "ROLE_REVIEWER"),
		viewer:   w.member(owner, "a", "viewer@a.test", "ROLE_VIEWER")}
	w.startBuilder(2)
	return rw
}

func dev(extra map[string]any) map[string]any {
	m := map[string]any{"org": "a", "project": "sample", "workspace": "dev"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (w *runWorld) waitRun(number int, statuses ...string) map[string]any {
	w.t.Helper()
	return w.waitFor(dev(nil), number, statuses...)
}

// waitFor waits until run number of workspace ws has one of statuses.
func (w *runWorld) waitFor(ws map[string]any, number int, statuses ...string) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		req := map[string]any{"number": number}
		for k, v := range ws {
			req[k] = v
		}
		run := w.owner.must("RunService/GetRun", req)["run"].(map[string]any)
		if slices.Contains(statuses, run["status"].(string)) {
			return run
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("run %d is %v, want one of %v", number, run, statuses)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitingNumbers(c *client) (approvals []float64) {
	for _, r := range asList(c.must("RunService/ListWaiting", map[string]any{"org": "a"})["approvals"]) {
		approvals = append(approvals, r.(map[string]any)["number"].(float64))
	}
	return approvals
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func TestRunWaitsForTheWorkspaceThenApplies(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.owner.must("RunService/CreateRun", dev(nil))
	w.waitRun(1, "RUN_STATUS_NEEDS_APPROVAL")

	second := w.owner.must("RunService/CreateRun", dev(nil))["run"].(map[string]any)
	if second["status"] != "RUN_STATUS_QUEUED" {
		t.Fatalf("second run: %v", second["status"])
	}
	time.Sleep(3 * time.Second)
	w.waitRun(2, "RUN_STATUS_QUEUED")

	if got := waitingNumbers(w.reviewer); !slices.Equal(got, []float64{1}) {
		t.Errorf("waiting for the reviewer: %v", got)
	}
	if got := waitingNumbers(w.viewer); len(got) != 0 {
		t.Errorf("waiting for the viewer: %v", got)
	}
	if _, code := w.viewer.call("RunService/ApproveRun", dev(map[string]any{"number": 1})); code != "permission_denied" {
		t.Errorf("a viewer approved: %q", code)
	}
	approved := w.reviewer.must("RunService/ApproveRun", dev(map[string]any{"number": 1}))["run"].(map[string]any)
	if approved["status"] != "RUN_STATUS_APPLYING" || approved["decidedBy"] != "reviewer@a.test" {
		t.Errorf("approved run: %v", approved)
	}
	if got := waitingNumbers(w.reviewer); len(got) != 0 {
		t.Errorf("still waiting after the approval: %v", got)
	}

	if run := w.waitRun(1, "RUN_STATUS_APPLIED", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_APPLIED" {
		t.Fatalf("first run: %v", run)
	}
	run := w.waitRun(2, "RUN_STATUS_PLANNED", "RUN_STATUS_NEEDS_APPROVAL", "RUN_STATUS_FAILED")
	if run["status"] != "RUN_STATUS_PLANNED" || run["hasChanges"] == true {
		t.Errorf("second run, after the first applied: %v", run)
	}
}

func TestRunRefusesAStalePlan(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.owner.must("RunService/CreateRun", dev(nil))
	w.waitRun(1, "RUN_STATUS_NEEDS_APPROVAL")

	token := w.token(w.owner, "a")
	if code, body := w.backend(http.MethodPost, "/state/a/sample/dev", token, stateDoc(1, "L1", "other")); code != http.StatusOK {
		t.Fatalf("write state: %d %s", code, body)
	}
	run := w.reviewer.must("RunService/ApproveRun", dev(map[string]any{"number": 1}))["run"].(map[string]any)
	if run["status"] != "RUN_STATUS_FAILED" || !strings.Contains(run["error"].(string), "plan is stale") {
		t.Errorf("approve after a state change: %v", run)
	}
}

func TestRunCancelFreesTheWorkspace(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.owner.must("RunService/CreateRun", dev(nil))
	w.waitRun(1, "RUN_STATUS_NEEDS_APPROVAL")
	w.owner.must("RunService/CreateRun", dev(nil))
	if _, code := w.viewer.call("RunService/CancelRun", dev(map[string]any{"number": 1})); code != "permission_denied" {
		t.Errorf("a viewer canceled: %q", code)
	}
	if run := w.owner.must("RunService/CancelRun", dev(map[string]any{"number": 1}))["run"].(map[string]any); run["status"] != "RUN_STATUS_CANCELED" {
		t.Errorf("canceled run: %v", run)
	}
	w.waitRun(2, "RUN_STATUS_NEEDS_APPROVAL")
}

func (c *client) runs() stackwrightv1connect.RunServiceClient {
	return stackwrightv1connect.NewRunServiceClient(c.http, c.w.srv.URL, connect.WithInterceptors(csrfHeader{c}))
}

// csrfHeader copies the CSRF cookie into its header, as the web app does.
type csrfHeader struct{ c *client }

func (h csrfHeader) set(header http.Header) {
	u, _ := url.Parse(h.c.w.srv.URL)
	for _, ck := range h.c.http.Jar.Cookies(u) {
		if ck.Name == auth.CSRFCookie {
			header.Set(auth.CSRFHeader, ck.Value)
		}
	}
}

func (h csrfHeader) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.set(req.Header())
		return next(ctx, req)
	}
}

func (h csrfHeader) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		h.set(conn.RequestHeader())
		return conn
	}
}

func (csrfHeader) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

type received struct {
	event *v1.RunEvent
	at    time.Time
}

// watch reads the run's timeline after cursor. It stops after the first
// engine event when stopEarly is set, or when the server ends the stream.
func watch(t *testing.T, c *client, cursor int64, stopEarly bool) ([]received, *v1.Run) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	stream, err := c.runs().WatchRun(ctx, connect.NewRequest(&v1.WatchRunRequest{Org: "a", Project: "sample", Workspace: "dev", Number: 1, AfterSeq: cursor}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var got []received
	var run *v1.Run
	for stream.Receive() {
		now := time.Now()
		if r := stream.Msg().GetRun(); r != nil {
			run = r
		}
		for _, e := range stream.Msg().GetEvents() {
			got = append(got, received{e, now})
			if stopEarly && strings.HasPrefix(e.GetType(), "engine.") {
				return got, run
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	return got, run
}

func TestRunTimelineIsLiveAndReplays(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.owner.must("RunService/CreateRun", dev(map[string]any{"planOnly": true}))

	first, _ := watch(t, w.viewer, 0, true)
	if len(first) == 0 {
		t.Fatal("no events before the first engine event")
	}
	rest, run := watch(t, w.viewer, first[len(first)-1].event.GetSeq(), false)
	if run.GetStatus() != v1.RunStatus_RUN_STATUS_PLANNED {
		t.Errorf("the stream ended with the run %v", run.GetStatus())
	}
	all := append(first, rest...)
	var slowest time.Duration
	for i, r := range all {
		if r.event.GetSeq() != int64(i+1) {
			t.Fatalf("event %d has seq %d: a gap or a repeat", i+1, r.event.GetSeq())
		}
		if strings.HasPrefix(r.event.GetType(), "engine.") {
			slowest = max(slowest, r.at.Sub(r.event.GetAt().AsTime()))
		}
	}
	if all[len(all)-1].event.GetSeq() != run.GetLastSeq() {
		t.Errorf("read %d events, the run has %d", len(all), run.GetLastSeq())
	}
	t.Logf("%d events; the slowest engine event arrived %s after the executor emitted it", len(all), slowest)
	if slowest > 2*time.Second {
		t.Errorf("an event took %s to arrive", slowest)
	}

	stream, err := w.client().runs().WatchRun(context.Background(), connect.NewRequest(&v1.WatchRunRequest{Org: "a", Project: "sample", Workspace: "dev", Number: 1}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		t.Error("a signed-out caller got an event")
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Errorf("signed out: %v", stream.Err())
	}
}

func TestRunPlansAnUpload(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	w := start(t, nil, true)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	w.startBuilder(1)
	ws := map[string]any{"org": "a", "project": "net", "workspace": "prod"}

	if _, code := owner.call("RunService/CreateRun", ws); code != "invalid_argument" {
		t.Errorf("no upload for a project that is not a sample: %q", code)
	}
	bad := map[string]any{"source": base64.StdEncoding.EncodeToString([]byte("not a tarball"))}
	for k, v := range ws {
		bad[k] = v
	}
	if _, code := owner.call("RunService/CreateRun", bad); code != "invalid_argument" {
		t.Errorf("an upload that is not gzip: %q", code)
	}

	upload := map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "ok")), "sourceName": "ok.tar.gz", "planOnly": true}
	for k, v := range ws {
		upload[k] = v
	}
	owner.must("RunService/CreateRun", upload)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		run := owner.must("RunService/GetRun", map[string]any{"org": "a", "project": "net", "workspace": "prod", "number": 1})["run"].(map[string]any)
		if run["status"] == "RUN_STATUS_PLANNED" || run["status"] == "RUN_STATUS_FAILED" {
			if run["status"] != "RUN_STATUS_PLANNED" || run["adds"] != float64(2) || run["source"] != "ok.tar.gz" {
				t.Errorf("planned upload: %v", run)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run: %v", run)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

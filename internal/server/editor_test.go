package server_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The block's empty line keeps two spaces, where completion is asked.
const natGateway = `terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}

resource "aws_nat_gateway" "main" {
` + "  \n}\n"

// editor calls an editor procedure, and tries again while the session's
// sandbox starts.
func (w *agentWorld) editor(procedure string, req map[string]any) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		out, code := w.owner.call(procedure, merge(map[string]any{"org": "a"}, req))
		switch {
		case code == "":
			return out
		case code != "unavailable" || time.Now().After(deadline):
			w.t.Fatalf("%s: %s %v", procedure, code, out["message"])
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (w *agentWorld) quietSession() string {
	w.t.Helper()
	s := w.owner.must("AgentService/StartSession", ws)["session"].(map[string]any)
	return s["id"].(string)
}

func TestEditorSessionNeedsNoModel(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	w.owner.must("AgentService/SetAgentSettings", map[string]any{"org": "a", "clearKey": true, "turnModel": "anthropic/claude-opus-5.5",
		"shortModel": "anthropic/claude-haiku-5.5", "monthlyCap": 50})
	id := w.quietSession()
	paths := w.editor("AgentService/ListFiles", map[string]any{"id": id})["paths"].([]any)
	// Each editor write makes one audit row, as TestEveryWriteMakesOneAuditRow
	// checks for the other writes.
	for _, call := range []struct {
		procedure string
		req       map[string]any
	}{
		{"AgentService/SaveFile", map[string]any{"id": id, "path": "notes.md", "content": "# notes\n"}},
		{"AgentService/OpenLanguageServer", map[string]any{"id": id}},
		{"AgentService/PlanCheckout", map[string]any{"id": id}},
	} {
		before := w.auditRows()
		w.editor(call.procedure, call.req)
		if got := w.auditRows() - before; got != 1 {
			t.Errorf("%s made %d audit rows, want 1", call.procedure, got)
		}
	}
	if !slices.Contains(paths, any("main.tf")) || slices.Contains(paths, any("terraform.tfstate")) {
		t.Errorf("paths: %v", paths)
	}
	if status, _, _ := w.status(id); status != "ready" {
		t.Errorf("the quiet session is %s after its sandbox started", status)
	}
	w.model.mu.Lock()
	calls := w.model.calls
	w.model.mu.Unlock()
	if calls != 0 {
		t.Errorf("a quiet session made %d model calls", calls)
	}
	if _, code := w.owner.call("AgentService/OpenFile", map[string]any{"org": "a", "id": id, "path": "terraform.tfstate"}); code != "permission_denied" {
		t.Errorf("opening a state file: %q", code)
	}
}

func TestEditorCompletesThroughTheSandbox(t *testing.T) {
	w := newAgentWorld(t, map[string]string{"main.tf": natGateway})
	id := w.quietSession()
	w.editor("AgentService/ListFiles", map[string]any{"id": id})
	ticket := w.editor("AgentService/OpenLanguageServer", map[string]any{"id": id})["ticket"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(w.srv.URL, "http")+"/lsp/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	// After an upgrade the body can be nil.
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(8 << 20)
	send := func(m map[string]any) {
		m["jsonrpc"] = "2.0"
		data, _ := json.Marshal(m)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(ticket)); err != nil {
		t.Fatal(err)
	}
	uri := "file:///repo/main.tf"
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"rootUri": "file:///repo", "capabilities": map[string]any{}}})
	// A client waits for initialize's answer before anything else.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no answer to initialize: %v", err)
		}
		var m struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(data, &m) == nil && m.ID == 1 {
			break
		}
	}
	send(map[string]any{"method": "initialized", "params": map[string]any{}})
	send(map[string]any{"method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "opentofu", "version": 1, "text": natGateway}}})
	// tofu-ls answers with no items until it has read the file, so the
	// completion is asked again, as an editor would.
	next := 2
	complete := func() {
		send(map[string]any{"id": next, "method": "textDocument/completion", "params": map[string]any{
			"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 7, "character": 2}}})
	}
	complete()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no completion: %v", err)
		}
		var m struct {
			ID     int `json:"id"`
			Result struct {
				Items []struct {
					Label string `json:"label"`
				} `json:"items"`
			} `json:"result"`
		}
		_ = json.Unmarshal(data, &m)
		if m.ID != next {
			if strings.Contains(string(data), "file:///work") {
				t.Errorf("a frame names the sandbox's path: %.200s", data)
			}
			continue
		}
		if len(m.Result.Items) == 0 {
			time.Sleep(time.Second)
			next++
			complete()
			continue
		}
		var labels []string
		for _, it := range m.Result.Items {
			labels = append(labels, it.Label)
		}
		for _, want := range []string{"subnet_id", "allocation_id", "connectivity_type"} {
			if !slices.Contains(labels, want) {
				t.Errorf("completion lacks %s: %v", want, labels)
			}
		}
		return
	}
}

func TestEditorRefusesAWebSocketWithoutATicket(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	id := w.quietSession()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(w.srv.URL, "http")+"/lsp/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	// After an upgrade the body can be nil.
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()
	_ = conn.Write(ctx, websocket.MessageText, []byte("made-up"))
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("a made-up ticket: %v", err)
	}
}

func TestEditorSaveDuringATurnKeepsBothChanges(t *testing.T) {
	const opened = "# top\n" + "resource \"random_pet\" \"web\" {}\n" + "# bottom\n"
	files := map[string]string{"main.tf": labFiles["main.tf"], "pets.tf": opened}
	w := newAgentWorld(t, files)
	w.model.script(0,
		use("edit_file", map[string]any{"path": "pets.tf", "old": "# top\n", "new": "# top, changed by the agent\n"}),
		use("validate", map[string]any{}),
		use("read_file", map[string]any{"path": "pets.tf"}),
		say("Done."),
	)
	id := w.startSession("Change the top comment")
	deadline := time.Now().Add(4 * time.Minute)
	for !strings.Contains(w.transcript(id), `"name":"validate"`) {
		if time.Now().After(deadline) {
			t.Fatalf("validate never started:\n%s", w.transcript(id))
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The person's editor still holds the file from before the agent's edit.
	human := strings.Replace(opened, "# bottom\n", "# bottom, changed by a person\n", 1)
	out := w.editor("AgentService/SaveFile", map[string]any{"id": id, "path": "pets.tf", "base": opened, "content": human})
	both := "# top, changed by the agent\nresource \"random_pet\" \"web\" {}\n# bottom, changed by a person\n"
	if out["outcome"] != "SAVE_OUTCOME_MERGED" || out["content"] != both {
		t.Fatalf("save during the turn: %v", out)
	}
	w.waitStatus(id, "ready")
	if got := w.editor("AgentService/OpenFile", map[string]any{"id": id, "path": "pets.tf"})["content"]; got != both {
		t.Errorf("the checkout after the turn: %q", got)
	}
}

func TestEditorSaveBetweenToolCallsReachesTheModel(t *testing.T) {
	const opened = "# top\n" + "resource \"random_pet\" \"web\" {}\n" + "# bottom\n"
	w := newAgentWorld(t, map[string]string{"main.tf": labFiles["main.tf"], "pets.tf": opened})
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	w.model.mu.Lock()
	w.model.hold = func(call int) {
		if call == 2 {
			<-release
		}
	}
	w.model.mu.Unlock()
	w.model.script(0,
		use("edit_file", map[string]any{"path": "pets.tf", "old": "# top\n", "new": "# top, changed by the agent\n"}),
		use("read_file", map[string]any{"path": "pets.tf"}),
		say("Done."),
	)
	id := w.startSession("Change the top comment")
	deadline := time.Now().Add(4 * time.Minute)
	for len(w.results(id)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("edit_file never ran:\n%s", w.transcript(id))
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The turn waits on its second model call; the save lands now.
	human := strings.Replace(opened, "# bottom\n", "# bottom, changed by a person\n", 1)
	out := w.editor("AgentService/SaveFile", map[string]any{"id": id, "path": "pets.tf", "base": opened, "content": human})
	open()
	both := "# top, changed by the agent\nresource \"random_pet\" \"web\" {}\n# bottom, changed by a person\n"
	if out["outcome"] != "SAVE_OUTCOME_MERGED" {
		t.Fatalf("save: %v", out)
	}
	w.waitStatus(id, "ready")
	results := w.results(id)
	if got := results[len(results)-1]["output"]; got != both {
		t.Errorf("the agent read %q", got)
	}
	w.model.mu.Lock()
	last := w.model.requests[len(w.model.requests)-1].Messages
	w.model.mu.Unlock()
	told := false
	for _, m := range last {
		told = told || (m.Role == "user" && strings.Contains(m.Content, "saved pets.tf"))
	}
	if !told {
		t.Errorf("the model was not told of the save: %+v", last)
	}
}

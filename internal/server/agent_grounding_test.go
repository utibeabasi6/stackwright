package server_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stackwright/stackwright/internal/agent"
)

func TestAgentReadsSchemasAndTheRepositorysRules(t *testing.T) {
	w := newAgentWorld(t, map[string]string{
		"main.tf": labFiles["main.tf"] + `
module "names" {
  source = "./modules/names"
  prefix = "x"
}
`,
		"modules/names/main.tf": `variable "prefix" { type = string }
resource "random_pet" "this" { prefix = var.prefix }
`,
		"STACKWRIGHT.md":    "Name every pet with the prefix acme.",
		"terraform.tfstate": labFiles["terraform.tfstate"],
	})
	w.agent.HiddenTools = []string{"module_docs"}
	w.model.script(0.01,
		use("provider_schema", map[string]any{"type": "random_password"}),
		use("provider_schema", map[string]any{"type": "random_pets"}),
		use("module_docs", map[string]any{"name": "names"}),
		use("conventions", map[string]any{}),
	)
	id := w.startSession("Add a password.")
	w.waitStatus(id, "ready")

	results := w.results(id)
	if len(results) != 4 {
		t.Fatalf("%d results:\n%s", len(results), w.transcript(id))
	}
	for i, want := range []string{
		"length: number, required",
		"No installed provider has a resource random_pets. Similar types: random_pet.",
		`There is no tool "module_docs".`,
		`There is no tool "conventions".`,
	} {
		if out, _ := results[i]["output"].(string); !strings.Contains(out, want) {
			t.Errorf("result %d: want %q in:\n%s", i, want, out)
		}
	}

	w.model.mu.Lock()
	reqs := slices.Clone(w.model.requests)
	w.model.mu.Unlock()
	if len(reqs) == 0 {
		t.Fatal("no model calls")
	}
	system := reqs[0].Messages[0].Content
	for _, want := range []string{"STACKWRIGHT.md:\nName every pet with the prefix acme.", "Provider random = { source = \"hashicorp/random\" } (main.tf)",
		"Module names from \"./modules/names\" (main.tf)", "read its arguments with provider_schema"} {
		if !strings.Contains(system, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, marker) {
		t.Error("the system prompt holds a state value")
	}
	for _, r := range reqs {
		if slices.ContainsFunc(r.Tools, func(t agent.Tool) bool { return t.Function.Name == "module_docs" }) {
			t.Fatal("a hidden tool was offered")
		}
	}
}

func TestAgentKeepsItsSandboxAcrossARestart(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	const note = "# kept across a restart\n"
	w.model.script(0,
		use("write_file", map[string]any{"path": "notes.tf", "content": note}),
		use("ask_user", map[string]any{"question": "Shall I go on?"}),
	)
	id := w.startSession("Write a note, then ask me")
	w.waitStatus(id, "waiting_for_answer")
	if _, dirty, sandbox := w.status(id); !dirty || sandbox == nil {
		t.Fatalf("before the restart: dirty=%v sandbox=%v", dirty, sandbox)
	}

	w.restartAgent(time.Minute)
	w.model.script(0, use("read_file", map[string]any{"path": "notes.tf"}), say("Done."))
	w.owner.must("AgentService/SendMessage", merge(ws, map[string]any{"id": id, "message": "Yes"}))
	w.waitStatus(id, "ready")
	results := w.results(id)
	if out, _ := results[len(results)-1]["output"].(string); out != note {
		t.Fatalf("the checkout after the restart: %q\n%s", out, w.transcript(id))
	}
	if _, dirty, sandbox := w.status(id); !dirty || sandbox == nil {
		t.Errorf("after the restart: dirty=%v sandbox=%v", dirty, sandbox)
	}
}

func TestAgentStartsAFreshSandboxWhenTheOldOneIsGone(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	w.model.script(0, say("Hi."))
	id := w.startSession("Say hi")
	w.waitStatus(id, "ready")
	// The toolbox cannot come back, as if its sandbox stopped while no
	// server ran: the row still names the sandbox, but its token is gone.
	if _, err := w.world.owner.Exec(context.Background(), `WITH gone AS (DELETE FROM agent_sandbox_tokens WHERE session_id = $1 RETURNING session_id, org_id)
		INSERT INTO agent_sandbox_tokens (token_hash, session_id, org_id) SELECT DISTINCT 'stale'::bytea, session_id, org_id FROM gone`, id); err != nil {
		t.Fatal(err)
	}

	w.restartAgent(2 * time.Second)
	w.model.script(0, use("list_files", map[string]any{}), say("Done."))
	w.owner.must("AgentService/SendMessage", merge(ws, map[string]any{"id": id, "message": "List the files"}))
	w.waitStatus(id, "ready")
	results := w.results(id)
	if r := results[len(results)-1]; r["error"] == true || !strings.Contains(fmt.Sprint(r["output"]), "main.tf") {
		t.Fatalf("after a fresh sandbox: %v\n%s", r, w.transcript(id))
	}
}

func TestAgentPushWorksOnceTheAppMayWrite(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	w.github.mu.Lock()
	w.github.pushFrom = 1 << 30
	w.github.mu.Unlock()
	w.model.script(0,
		use("write_file", map[string]any{"path": "pets.tf", "content": randomPetDB}),
		use("propose_commit", map[string]any{"message": "Add a random_pet named db"}),
	)
	id := w.startSession("Add a pet")
	w.waitStatus(id, "waiting_for_approval")
	seq, _ := w.proposal(id)
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "waiting_for_approval")
	if !strings.Contains(w.transcript(id), "Write access to repository not granted") {
		t.Fatalf("the first push did not fail as GitHub does:\n%s", w.transcript(id))
	}

	// The App's owner grants write; tokens minted from now on may push.
	w.github.mu.Lock()
	w.github.pushFrom = w.github.mints + 1
	w.github.mu.Unlock()
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "ready")
	branch := ""
	for ref := range w.github.refs("acme/infra") {
		if strings.HasPrefix(ref, "refs/heads/agent/") {
			branch = ref
		}
	}
	if branch == "" || w.github.show("acme/infra", branch, "pets.tf") != randomPetDB {
		t.Fatalf("the retry did not push the kept commit; branch %q\n%s", branch, w.transcript(id))
	}
}

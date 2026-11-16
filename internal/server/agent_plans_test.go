package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/utibeabasi6/stackwright/internal/agent"
)

const randomProvider = `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`

var brokenFiles = map[string]string{"main.tf": randomProvider + `
resource "random_pet" "web" {
  lenght = 2
}

output "name" {
  value = random_pet.missing.id
}
`}

type agentRun struct {
	number           int
	status, trigger  string
	planOnly         bool
	session, planJob string
}

func (w *agentWorld) runs() []agentRun {
	w.t.Helper()
	rows, err := w.world.owner.Query(context.Background(), `SELECT number, status, trigger, plan_only, coalesce(agent_session_id::text, ''),
		coalesce(plan_job_id::text, '') FROM runs ORDER BY number`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []agentRun
	for rows.Next() {
		var r agentRun
		if err := rows.Scan(&r.number, &r.status, &r.trigger, &r.planOnly, &r.session, &r.planJob); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestAgentPlansTheBrokenFixtureUntilItIsClean(t *testing.T) {
	w := newAgentWorld(t, brokenFiles)
	w.model.script(0,
		use("run_plan", map[string]any{}),
		use("edit_file", map[string]any{"path": "main.tf", "old": "lenght", "new": "length"}),
		use("run_plan", map[string]any{}),
		use("edit_file", map[string]any{"path": "main.tf", "old": "random_pet.missing.id", "new": "random_pet.web.id"}),
		use("run_plan", map[string]any{}),
		use("plan_details", map[string]any{"address": "random_pet.web"}),
		say("The plan is clean."),
	)
	id := w.startSession("Fix the code until the plan is clean")
	w.waitStatus(id, "ready")

	res := w.results(id)
	if len(res) != 6 {
		t.Fatalf("results:\n%s", w.transcript(id))
	}
	for i, r := range []map[string]any{res[0], res[2]} {
		if out := r["output"].(string); !strings.Contains(out, "failed") || !strings.Contains(out, "main.tf:") {
			t.Errorf("plan %d does not report its errors: %q", i+1, out)
		}
	}
	if out := res[4]["output"].(string); !strings.Contains(out, "Run #3 planned: 1 to add") || !strings.Contains(out, "create random_pet.web") {
		t.Errorf("third plan: %q", out)
	}
	if out := res[5]["output"].(string); !strings.Contains(out, "random_pet.web: create") || !strings.Contains(out, "length: (none) -> 2") {
		t.Errorf("plan_details: %q", out)
	}

	runs := w.runs()
	if len(runs) != 3 {
		t.Fatalf("runs: %+v", runs)
	}
	for i, r := range runs {
		want := "failed"
		if i == 2 {
			want = "planned"
		}
		if r.status != want || r.trigger != "agent" || !r.planOnly || r.session != id {
			t.Errorf("run %d: %+v", r.number, r)
		}
	}

	// No path applies an agent plan: the API, a status forced in the
	// database, or an apply job written by hand.
	if _, code := w.owner.call("RunService/ApproveRun", merge(ws, map[string]any{"number": 3})); code != "failed_precondition" {
		t.Errorf("ApproveRun on an agent plan: %q", code)
	}
	ctx := context.Background()
	_, err := w.world.owner.Exec(ctx, `UPDATE runs SET status = 'needs_approval' WHERE number = 3`)
	checkViolation(t, "a waiting agent run", err)
	_, err = w.world.owner.Exec(ctx, `UPDATE runs SET plan_only = false WHERE number = 3`)
	checkViolation(t, "an agent run that is not plan-only", err)
	_, err = w.world.owner.Exec(ctx, `INSERT INTO jobs (id, org_id, workspace_id, operation, plan_job_id, engine, engine_version, platform,
		artifact_key, deadline, run_id) SELECT gen_random_uuid(), org_id, workspace_id, 'apply', id, engine, engine_version, platform, artifact_key,
		deadline, run_id FROM jobs WHERE id = $1`, runs[2].planJob)
	checkViolation(t, "an apply job for an agent plan", err)
}

func checkViolation(t *testing.T, what string, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Errorf("%s: want a check violation, got %v", what, err)
	}
}

// TestAgentPackageHasNoPathToApply checks the agent's imports: it reaches
// runs, jobs, state and GitHub only through hooks (adr/0031).
func TestAgentPackageHasNoPathToApply(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/utibeabasi6/stackwright/internal/agent").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"api", "server", "runs", "jobs", "state", "github", "executor", "toolbox"} {
			if dep == "github.com/utibeabasi6/stackwright/internal/"+banned {
				t.Errorf("the agent package imports %s", dep)
			}
		}
	}
}

const secretFiles = randomProvider + `
variable "db_password" {
  type      = string
  sensitive = true
}

resource "random_password" "db" {
  length = 20
}

resource "terraform_data" "conf" {
  input = { password = var.db_password, user = "app" }
}
`

func TestAgentNeverSeesSensitiveValues(t *testing.T) {
	w := newAgentWorld(t, map[string]string{"main.tf": secretFiles})
	const secret = "hunter2-agent-secret"
	w.owner.must("VariableService/SetVariable", map[string]any{"org": "a", "scope": map[string]any{"project": "infra", "workspace": "default"},
		"category": "VARIABLE_CATEGORY_TERRAFORM", "key": "db_password", "value": secret, "sensitive": true})

	const sha = "5ec4e7000000000000000000000000000000beef"
	w.github.mu.Lock()
	w.github.heads["main"] = sha
	w.github.mu.Unlock()
	w.github.commit(sha, map[string]string{"main.tf": secretFiles})
	w.owner.must("RunService/CreateRun", ws)
	rw := &runWorld{world: w.world, owner: w.owner}
	if run := rw.waitFor(ws, 1, "RUN_STATUS_NEEDS_APPROVAL", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_NEEDS_APPROVAL" {
		t.Fatalf("plan: %v", run)
	}
	w.owner.must("RunService/ApproveRun", merge(ws, map[string]any{"number": 1}))
	if run := rw.waitFor(ws, 1, "RUN_STATUS_APPLIED", "RUN_STATUS_FAILED"); run["status"] != "RUN_STATUS_APPLIED" {
		t.Fatalf("apply: %v", run)
	}
	latest := w.owner.must("StateService/GetState", ws)["latest"].(map[string]any)
	var state struct {
		Resources []struct {
			Type      string
			Instances []struct{ Attributes map[string]any }
		}
	}
	raw := w.owner.must("StateService/GetStateVersion", merge(ws, map[string]any{"id": latest["id"]}))["state"].(string)
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	var generated string
	for _, r := range state.Resources {
		if r.Type == "random_password" {
			generated, _ = r.Instances[0].Attributes["result"].(string)
		}
	}
	if len(generated) != 20 {
		t.Fatalf("no random password in the state: %s", raw)
	}

	changed := strings.NewReplacer("length = 20", "length = 24", `user = "app"`, `user = "admin"`).Replace(secretFiles)
	w.model.script(0,
		use("write_file", map[string]any{"path": "main.tf", "content": changed}),
		use("run_plan", map[string]any{}),
		use("plan_details", map[string]any{"address": "random_password.db"}),
		use("plan_details", map[string]any{"address": "terraform_data.conf"}),
		use("state_lookup", map[string]any{"address": "random_password.db"}),
		use("state_lookup", map[string]any{"address": "terraform_data.conf"}),
		say("Done."),
	)
	id := w.startSession("Make the password longer and the user admin")
	w.waitStatus(id, "ready")
	res := w.results(id)
	if len(res) != 6 {
		t.Fatalf("results:\n%s", w.transcript(id))
	}
	for _, c := range []struct {
		i    int
		want []string
	}{
		{1, []string{"Run #2 planned: 1 to add, 1 to change, 1 to destroy", "replace random_password.db"}},
		{2, []string{"random_password.db: replace", "result: (sensitive) -> (known after apply)", "length: 20 -> 24 (forces replacement)"}},
		{3, []string{"terraform_data.conf: update", `input.user: "app" -> "admin"`}},
		{4, []string{"result = (sensitive)", "length = 20"}},
		{5, []string{"input.password = (sensitive)", `input.user = "app"`}},
	} {
		out := res[c.i]["output"].(string)
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("result %d lacks %q:\n%s", c.i, want, out)
			}
		}
	}
	transcript := w.transcript(id)
	for _, s := range w.steps(id) {
		b, _ := json.Marshal(s.Payload)
		transcript += string(b)
	}
	for _, leaked := range []string{secret, generated} {
		if strings.Contains(transcript, leaked) {
			t.Errorf("the transcript holds a sensitive value %q", leaked)
		}
	}
}

func TestAgentPullRequestNeedsAnApprovalAndNamesTheApprover(t *testing.T) {
	w := newAgentWorld(t, labFiles)
	w.model.script(0,
		use("propose_pull_request", map[string]any{"title": "Too early", "body": "Nothing is pushed."}),
		use("write_file", map[string]any{"path": "db.tf", "content": randomPetDB}),
		use("propose_commit", map[string]any{"message": "Add a random_pet named db"}),
	)
	id := w.startSession("Add a random_pet named db and open a pull request")
	w.waitStatus(id, "waiting_for_approval")
	if out := w.results(id)[0]; out["error"] != true || !strings.Contains(out["output"].(string), "no pushed commit") {
		t.Errorf("a pull request before a push: %v", out)
	}
	seq, _ := w.proposal(id)
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "ready")

	w.model.script(0, use("propose_pull_request", map[string]any{"title": "Add a random_pet named db", "body": "Adds `random_pet.db`."}))
	w.owner.must("AgentService/SendMessage", merge(ws, map[string]any{"id": id, "message": "Open the pull request"}))
	w.waitStatus(id, "waiting_for_approval")
	w.github.mu.Lock()
	early := len(w.github.pulls)
	w.github.mu.Unlock()
	if early != 0 {
		t.Fatal("a pull request opened before an approval")
	}
	refs := w.github.refs("acme/infra")
	seq, proposal := w.proposal(id)
	if proposal["type"] != "pull_request" || proposal["title"] != "Add a random_pet named db" {
		t.Fatalf("proposal: %v", proposal)
	}
	session := w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))["session"]
	if session.(map[string]any)["status"] != "AGENT_SESSION_STATUS_WORKING" {
		t.Errorf("session after the decision: %v", session)
	}
	w.waitStatus(id, "ready")

	w.github.mu.Lock()
	pulls := slices.Clone(w.github.pulls)
	w.github.mu.Unlock()
	var branch string
	var number int
	if err := w.world.owner.QueryRow(context.Background(), `SELECT branch, coalesce(pr_number, 0) FROM agent_sessions WHERE id = $1`, id).
		Scan(&branch, &number); err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 1 || number != 1 {
		t.Fatalf("pulls %+v, session's number %d\n%s", pulls, number, w.transcript(id))
	}
	p := pulls[0]
	if p.Path != "/repos/acme/infra/pulls" || p.Head != branch || p.Base != "main" || p.Draft || !strings.HasPrefix(p.Token, "ghs_") ||
		!strings.Contains(p.Body, "Adds `random_pet.db`.") || !strings.Contains(p.Body, "Owner approved opening it.") {
		t.Errorf("pull request: %+v", p)
	}
	if after := w.github.refs("acme/infra"); !maps.Equal(after, refs) {
		t.Errorf("opening the pull request moved a ref: %v -> %v", refs, after)
	}

	w.model.script(0, use("propose_pull_request", map[string]any{"title": "Again", "body": "Again."}), say("It is open."))
	w.owner.must("AgentService/SendMessage", merge(ws, map[string]any{"id": id, "message": "Open another"}))
	w.waitStatus(id, "ready")
	if res := w.results(id); !strings.Contains(res[len(res)-1]["output"].(string), "#1 is open already") {
		t.Errorf("a second pull request: %v", res[len(res)-1])
	}
}

// TestAgentPlansRealModel asserts what must hold whatever a real model
// says: agent runs stay plan-only, a turn runs at most 5 plans, and
// nothing reaches GitHub without an approval.
func TestAgentPlansRealModel(t *testing.T) {
	key := os.Getenv("STACKWRIGHT_TEST_OPENROUTER_KEY")
	if key == "" {
		t.Skip("set STACKWRIGHT_TEST_OPENROUTER_KEY to run the agent against a real model")
	}
	w := newAgentWorld(t, brokenFiles)
	w.agent.Model = agent.OpenRouter{URL: "https://openrouter.ai/api/v1", HTTP: &http.Client{Timeout: 5 * time.Minute}}
	w.owner.must("AgentService/SetAgentSettings", map[string]any{"org": "a", "apiKey": key,
		"turnModel": "anthropic/claude-opus-5.5", "shortModel": "anthropic/claude-haiku-5.5", "monthlyCap": 3})
	before := w.github.refs("acme/infra")
	done := []string{"ready", "waiting_for_answer", "waiting_for_approval"}

	id := w.startSession("Plan this workspace. Fix the code until the plan is clean, then propose a commit.")
	w.waitStatusFor(id, 15*time.Minute, done...)
	runs := w.runs()
	clean := false
	for _, r := range runs {
		if r.trigger != "agent" || !r.planOnly || r.session != id {
			t.Errorf("run %d: %+v", r.number, r)
		}
		clean = clean || r.status == "planned"
	}
	t.Logf("%d plans, clean=%v", len(runs), clean)
	if len(runs) == 0 || len(runs) > 5 || !clean {
		t.Errorf("%d plans, clean=%v\n%s", len(runs), clean, w.transcript(id))
	}
	if after := w.github.refs("acme/infra"); !maps.Equal(after, before) {
		t.Fatalf("a push happened before an approval: %v", after)
	}
	seq, proposal := w.proposal(id)
	if proposal["type"] == "pull_request" {
		t.Fatalf("the first proposal is a pull request: %v", proposal)
	}
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "ready")

	w.owner.must("AgentService/SendMessage", merge(ws, map[string]any{"id": id, "message": "Open a pull request for this change."}))
	w.waitStatusFor(id, 10*time.Minute, done...)
	w.github.mu.Lock()
	early := len(w.github.pulls)
	w.github.mu.Unlock()
	if early != 0 {
		t.Fatal("a pull request opened before an approval")
	}
	seq, proposal = w.proposal(id)
	if proposal["type"] != "pull_request" {
		t.Fatalf("no pull request proposed: %v\n%s", proposal, w.transcript(id))
	}
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "ready")
	w.github.mu.Lock()
	pulls := slices.Clone(w.github.pulls)
	w.github.mu.Unlock()
	if len(pulls) != 1 || !strings.Contains(pulls[0].Body, "Owner approved opening it.") {
		t.Errorf("pulls: %+v", pulls)
	} else {
		t.Logf("pull request %q:\n%s", pulls[0].Title, pulls[0].Body)
	}

	var spent float64
	var calls int
	if err := w.world.owner.QueryRow(context.Background(), `SELECT coalesce(sum(cost), 0)::float8, count(*) FILTER (WHERE kind = 'assistant')
		FROM agent_steps`).Scan(&spent, &calls); err != nil {
		t.Fatal(err)
	}
	t.Logf("spent $%.4f over %d model calls\n%s", spent, calls, w.transcript(id))
}

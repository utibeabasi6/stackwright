package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utibeabasi6/stackwright/internal/agent"
)

// The eval runs each task in evals/tasks against a real model and scores
// it by a plan of the commit that the agent proposes (adr/0032). It spends
// money, so it runs only when STACKWRIGHT_EVAL is set; make eval sets it.

type evalTask struct {
	Prompt string `json:"prompt"`
	// Base names a folder in evals/base whose files are added first.
	Base   string       `json:"base"`
	Expect []evalExpect `json:"expect"`
	// Absent lists resource types that the plan must not create.
	Absent []string `json:"absent"`
}

type evalExpect struct {
	Type string `json:"type"`
	// Count is how many instances must match; 0 means at least one.
	Count int `json:"count"`
	// Values are dotted paths into the planned values, such as
	// "versioning_configuration.0.status".
	Values map[string]any `json:"values"`
}

type evalResult struct {
	Task          string         `json:"task"`
	Passed        bool           `json:"passed"`
	Reason        string         `json:"reason,omitempty"`
	Cost          float64        `json:"cost"`
	ModelCalls    int            `json:"model_calls"`
	Plans         int            `json:"plans"`
	ArgumentError int            `json:"argument_errors"`
	ToolCalls     map[string]int `json:"tool_calls"`
	Seconds       int            `json:"seconds"`
}

// argumentError matches the engine's diagnostics for an argument, block
// or type that the schema does not have, or one it needs.
var argumentError = regexp.MustCompile(`Unsupported argument|Missing required argument|Unsupported block type|Missing required block|` +
	`Insufficient \S+ blocks|Too many \S+ blocks|Invalid resource type|Invalid data source|Unsupported attribute|` +
	`Incorrect attribute value type|Conflicting configuration arguments|Invalid combination of arguments`)

func TestEval(t *testing.T) {
	key := os.Getenv("STACKWRIGHT_TEST_OPENROUTER_KEY")
	if os.Getenv("STACKWRIGHT_EVAL") == "" || key == "" {
		t.Skip("set STACKWRIGHT_EVAL=1 and STACKWRIGHT_TEST_OPENROUTER_KEY to run the eval; it spends money")
	}
	arm := os.Getenv("STACKWRIGHT_EVAL_ARM")
	var hidden []string
	switch arm {
	case "grounded":
	case "baseline":
		hidden = []string{"provider_schema", "module_docs"}
	default:
		t.Fatalf("STACKWRIGHT_EVAL_ARM is %q; want baseline or grounded", arm)
	}
	model := os.Getenv("STACKWRIGHT_EVAL_MODEL")
	if model == "" {
		model = "anthropic/claude-opus-5.5"
	}
	only := regexp.MustCompile(os.Getenv("STACKWRIGHT_EVAL_TASKS"))
	dirs, err := filepath.Glob("../../evals/tasks/*/task.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var results []evalResult
	t.Run("tasks", func(t *testing.T) {
		for _, path := range dirs {
			name := filepath.Base(filepath.Dir(path))
			if !only.MatchString(name) {
				continue
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := runEvalTask(t, filepath.Dir(path), key, model, hidden)
				r.Task = name
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				if !r.Passed {
					t.Logf("failed: %s", r.Reason)
				}
			})
		}
	})
	slices.SortFunc(results, func(a, b evalResult) int { return strings.Compare(a.Task, b.Task) })
	var passed, argErrors int
	var cost float64
	for _, r := range results {
		if r.Passed {
			passed++
		}
		argErrors += r.ArgumentError
		cost += r.Cost
	}
	summary := map[string]any{"arm": arm, "model": model, "date": time.Now().UTC().Format(time.RFC3339),
		"score": fmt.Sprintf("%d/%d", passed, len(results)), "argument_errors": argErrors, "cost": cost, "tasks": results}
	t.Logf("%s on %s: %d of %d tasks passed, %d argument errors, $%.2f", arm, model, passed, len(results), argErrors, cost)
	if out := os.Getenv("STACKWRIGHT_EVAL_OUT"); out != "" {
		data, _ := json.MarshalIndent(summary, "", "  ")
		if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readEvalTask(t *testing.T, dir string) (evalTask, map[string]string) {
	t.Helper()
	var task evalTask
	raw, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	files := map[string]string{}
	add := func(root string) {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "solution":
				return filepath.SkipDir
			case d.IsDir() || d.Name() == "task.json":
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Base != "" {
		add(filepath.Join(dir, "..", "..", "base", task.Base))
	}
	add(dir)
	return task, files
}

func runEvalTask(t *testing.T, dir, key, model string, hidden []string) (r evalResult) {
	start := time.Now()
	task, files := readEvalTask(t, dir)
	w := newAgentWorld(t, files)
	w.agent.Model = agent.OpenRouter{URL: "https://openrouter.ai/api/v1", HTTP: &http.Client{Timeout: 5 * time.Minute}}
	w.agent.HiddenTools = hidden
	w.owner.must("AgentService/SetAgentSettings", map[string]any{"org": "a", "apiKey": key,
		"turnModel": model, "shortModel": "anthropic/claude-haiku-5.5", "monthlyCap": 3})

	id := w.startSession(task.Prompt + "\n\nMake reasonable choices without asking. When the plan is clean, propose a commit.")
	w.waitStatusFor(id, 35*time.Minute, "ready", "waiting_for_answer", "waiting_for_approval")
	r = evalResult{ToolCalls: map[string]int{}}
	defer func() {
		r.Seconds = int(time.Since(start).Seconds())
		if !r.Passed {
			t.Logf("transcript:\n%s", w.transcript(id))
		}
		// STACKWRIGHT_EVAL_TRANSCRIPTS keeps every step in full, one JSON
		// object per line, for reading failures later.
		if out := os.Getenv("STACKWRIGHT_EVAL_TRANSCRIPTS"); out != "" {
			var b strings.Builder
			for _, s := range w.steps(id) {
				line, _ := json.Marshal(s)
				b.Write(append(line, '\n'))
			}
			if err := os.WriteFile(filepath.Join(out, filepath.Base(dir)+".jsonl"), []byte(b.String()), 0o600); err != nil {
				t.Error(err)
			}
		}
	}()
	names := map[string]string{}
	for _, s := range w.steps(id) {
		switch s.Kind {
		case "assistant":
			r.ModelCalls++
		case "tool_call":
			callID, _ := s.Payload["id"].(string)
			name, _ := s.Payload["name"].(string)
			names[callID] = name
			r.ToolCalls[name]++
		case "tool_result":
			callID, _ := s.Payload["id"].(string)
			if n := names[callID]; n == "validate" || n == "run_plan" {
				out, _ := s.Payload["output"].(string)
				r.ArgumentError += len(argumentError.FindAllString(out, -1))
			}
		}
	}
	r.Plans = r.ToolCalls["run_plan"]
	if err := w.world.owner.QueryRow(context.Background(), `SELECT coalesce(sum(cost), 0)::float8 FROM agent_steps WHERE session_id = $1`, id).
		Scan(&r.Cost); err != nil {
		t.Fatal(err)
	}
	status, _, _ := w.status(id)
	if status != "waiting_for_approval" {
		r.Reason = "the agent ended without a proposal; status " + status
		return r
	}
	seq, proposal := w.proposal(id)
	if proposal["type"] == "pull_request" {
		r.Reason = "the agent proposed a pull request, not a commit"
		return r
	}
	w.owner.must("AgentService/DecideProposal", merge(ws, map[string]any{"id": id, "proposal": seq, "approve": true}))
	w.waitStatus(id, "ready")

	final := map[string]string{}
	for ref, sha := range w.github.refs("acme/infra") {
		if !strings.HasPrefix(ref, "refs/heads/agent/") {
			continue
		}
		out, err := exec.Command("git", "--git-dir", filepath.Join(w.github.gitRoot, "acme/infra.git"), "ls-tree", "-r", "--name-only", sha).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range strings.Fields(string(out)) {
			final[f] = w.github.show("acme/infra", sha, f)
		}
		w.github.mu.Lock()
		w.github.heads["main"] = sha
		w.github.mu.Unlock()
		w.github.commit(sha, final)
	}
	if len(final) == 0 {
		r.Reason = "nothing was pushed to the session's branch"
		return r
	}

	number := int(w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"planOnly": true}))["run"].(map[string]any)["number"].(float64))
	deadline := time.Now().Add(15 * time.Minute)
	for {
		run := w.owner.must("RunService/GetRun", merge(ws, map[string]any{"number": number}))["run"].(map[string]any)
		switch run["status"] {
		case "RUN_STATUS_PLANNED":
		case "RUN_STATUS_FAILED", "RUN_STATUS_CANCELED":
			r.Reason = fmt.Sprintf("the plan of the commit failed: %v", run["error"])
			return r
		default:
			if time.Now().After(deadline) {
				r.Reason = "the plan of the commit did not finish"
				return r
			}
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	var planJob string
	for _, run := range w.runs() {
		if run.number == number {
			planJob = run.planJob
		}
	}
	plan := (&jobWorld{world: w.world}).artifact(planJob, "plan.json")
	r.Reason = checkPlan(plan, task)
	r.Passed = r.Reason == ""
	return r
}

// checkPlan returns why the plan does not meet the task, or "".
func checkPlan(raw []byte, task evalTask) string {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Type    string `json:"type"`
			Mode    string `json:"mode"`
			Change  struct {
				Actions []string `json:"actions"`
				After   any      `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return "the plan is not JSON: " + err.Error()
	}
	var problems []string
	kept := func(actions []string) bool { return !slices.Equal(actions, []string{"delete"}) }
	for _, e := range task.Expect {
		matched, of := 0, 0
		for _, rc := range plan.ResourceChanges {
			if rc.Mode != "managed" || rc.Type != e.Type || !kept(rc.Change.Actions) {
				continue
			}
			of++
			ok := true
			for p, want := range e.Values {
				got, found := lookup(rc.Change.After, p)
				if !found || fmt.Sprint(got) != fmt.Sprint(want) {
					ok = false
					break
				}
			}
			if ok {
				matched++
			}
		}
		switch {
		case e.Count == 0 && matched == 0, e.Count > 0 && matched != e.Count:
			problems = append(problems, fmt.Sprintf("%s: %d of %d instances match %v, want %s", e.Type, matched, of, e.Values, want(e.Count)))
		}
	}
	for _, typ := range task.Absent {
		for _, rc := range plan.ResourceChanges {
			if rc.Mode == "managed" && rc.Type == typ && kept(rc.Change.Actions) {
				problems = append(problems, "the plan has "+rc.Address+", which the task rules out")
			}
		}
	}
	return strings.Join(problems, "; ")
}

func want(count int) string {
	if count == 0 {
		return "at least 1"
	}
	return strconv.Itoa(count)
}

// lookup follows a dotted path, with numbers for list items.
func lookup(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = x[part]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// TestEvalSolutions plans each task's seed and its reference solution with a
// local engine: the solution must pass the task's checks and the seed must
// not. It needs no model.
func TestEvalSolutions(t *testing.T) {
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to plan the eval's solutions")
	}
	engine, err := exec.LookPath("tofu")
	if err != nil {
		if engine, err = exec.LookPath("terraform"); err != nil {
			t.Skip("no tofu or terraform on PATH")
		}
	}
	paths, err := filepath.Glob("../../evals/tasks/*/task.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 30 {
		t.Errorf("%d tasks, want 30", len(paths))
	}
	cache := t.TempDir()
	plan := func(t *testing.T, files map[string]string) ([]byte, error) {
		dir := t.TempDir()
		for name, body := range files {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"init", "-input=false", "-no-color"}, {"plan", "-input=false", "-no-color", "-out=plan"}, {"show", "-json", "plan"}} {
			cmd := exec.Command(engine, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "TF_PLUGIN_CACHE_DIR="+cache, "TF_IN_AUTOMATION=1")
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w\n%s%s", filepath.Base(engine), args[0], err, out, stderr.String())
			}
			if args[0] == "show" {
				return out, nil
			}
		}
		return nil, nil
	}
	for _, path := range paths {
		dir := filepath.Dir(path)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			task, files := readEvalTask(t, dir)
			if strings.TrimSpace(task.Prompt) == "" || len(task.Expect) == 0 {
				t.Fatal("a task needs a prompt and at least one expectation")
			}
			if raw, err := plan(t, files); err == nil && checkPlan(raw, task) == "" {
				t.Error("the seed passes the task's checks before any change")
			}
			solution, err := os.ReadDir(filepath.Join(dir, "solution"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range solution {
				b, err := os.ReadFile(filepath.Join(dir, "solution", e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				files[e.Name()] = string(b)
			}
			raw, err := plan(t, files)
			if err != nil {
				t.Fatal(err)
			}
			if why := checkPlan(raw, task); why != "" {
				t.Errorf("the solution fails: %s", why)
			}
		})
	}
}

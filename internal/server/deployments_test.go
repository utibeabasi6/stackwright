package server_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// deploymentWorld has kms, secrets and eks, each planned once from its
// fixture so a deployment has their code.
func deploymentWorld(t *testing.T) *runWorld {
	t.Helper()
	w := newRunWorld(t)
	for _, name := range []string{"kms", "secrets", "eks"} {
		w.project(w.owner, "a", name, "prod")
		ws := map[string]any{"org": "a", "project": name, "workspace": "prod"}
		w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "deploy-"+name)),
			"sourceName": name + ".tar.gz", "planOnly": true}))
		// Without its inputs, a plan of secrets or eks fails; it only has
		// to leave the code behind.
		w.waitFor(ws, 1, "RUN_STATUS_PLANNED", "RUN_STATUS_FAILED")
	}
	return w
}

func (w *runWorld) waitDeployment(name string, number int, statuses ...string) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for {
		run := w.owner.must("DeploymentService/GetDeploymentRun", map[string]any{"org": "a", "name": name, "number": number})["run"].(map[string]any)
		for _, s := range statuses {
			if run["status"] == s {
				return run
			}
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("deployment run %d is %v, want one of %v", number, run, statuses)
		}
		time.Sleep(time.Second)
	}
}

func step(project string) map[string]any {
	return map[string]any{"project": project, "workspace": "prod"}
}

func TestDeploymentAppliesInOrderWithOneApproval(t *testing.T) {
	t.Parallel()
	w := deploymentWorld(t)
	w.owner.must("DeploymentService/SaveDeployment", map[string]any{"org": "a", "name": "platform",
		"steps": []any{step("eks"), step("secrets"), step("kms")},
		"links": []any{
			map[string]any{"from": step("kms"), "to": step("secrets"), "output": "key_arn", "variable": "kms_key_arn"},
			map[string]any{"from": step("secrets"), "to": step("eks"), "output": "secret_arn", "variable": "secret_arn"},
		}})
	if _, code := w.owner.call("DeploymentService/SaveDeployment", map[string]any{"org": "a", "name": "loop",
		"steps": []any{step("kms"), step("secrets")},
		"links": []any{map[string]any{"from": step("kms"), "to": step("secrets")}, map[string]any{"from": step("secrets"), "to": step("kms")}}}); code != "invalid_argument" {
		t.Errorf("a cycle: %q", code)
	}

	w.owner.must("DeploymentService/StartDeploymentRun", map[string]any{"org": "a", "name": "platform"})
	run := w.waitDeployment("platform", 1, "DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL")
	var order []string
	for _, s := range asList(run["steps"]) {
		order = append(order, s.(map[string]any)["workspace"].(map[string]any)["project"].(string))
	}
	if strings.Join(order, ",") != "kms,secrets,eks" {
		t.Errorf("order %v", order)
	}
	if _, code := w.viewer.call("DeploymentService/ApproveDeploymentRun", map[string]any{"org": "a", "name": "platform", "number": 1}); code != "permission_denied" {
		t.Errorf("a viewer approved: %q", code)
	}
	w.reviewer.must("DeploymentService/ApproveDeploymentRun", map[string]any{"org": "a", "name": "platform", "number": 1})
	w.waitDeployment("platform", 1, "DEPLOYMENT_RUN_STATUS_APPLIED")

	// Each step started after the one before it applied, and eks got the
	// secret's ARN through the secrets step.
	rows, err := w.world.owner.Query(context.Background(), `SELECT p.slug, r.created_at, r.finished_at FROM runs r
		JOIN deployment_runs d ON d.id = r.deployment_run_id JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id
		WHERE d.number = 1 AND r.status = 'applied' ORDER BY r.finished_at`)
	if err != nil {
		t.Fatal(err)
	}
	var applied []string
	var lastEnd time.Time
	for rows.Next() {
		var slug string
		var created, finished time.Time
		if err := rows.Scan(&slug, &created, &finished); err != nil {
			t.Fatal(err)
		}
		if !lastEnd.IsZero() && created.Before(lastEnd) {
			t.Errorf("%s started before the step before it applied", slug)
		}
		applied, lastEnd = append(applied, slug), finished
	}
	rows.Close()
	if strings.Join(applied, ",") != "kms,secrets,eks" {
		t.Errorf("applied %v", applied)
	}
	var outputs []byte
	if err := w.world.owner.QueryRow(context.Background(), `SELECT outputs FROM workspace_outputs o JOIN workspaces w ON w.id = o.workspace_id
		JOIN projects p ON p.id = w.project_id WHERE p.slug = 'eks'`).Scan(&outputs); err != nil || len(outputs) == 0 {
		t.Fatalf("eks has no outputs: %v", err)
	}

	// A failure in secrets stops eks and leaves kms applied.
	w.owner.must("VariableService/SetVariable", map[string]any{"org": "a", "scope": map[string]any{"project": "secrets"},
		"category": "VARIABLE_CATEGORY_TERRAFORM", "key": "fail", "value": "true"})
	w.owner.must("VariableService/SetVariable", map[string]any{"org": "a", "scope": map[string]any{"project": "eks"},
		"category": "VARIABLE_CATEGORY_TERRAFORM", "key": "bump", "value": "2"})
	w.owner.must("DeploymentService/StartDeploymentRun", map[string]any{"org": "a", "name": "platform"})
	w.waitDeployment("platform", 2, "DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL")
	w.reviewer.must("DeploymentService/ApproveDeploymentRun", map[string]any{"org": "a", "name": "platform", "number": 2})
	failed := w.waitDeployment("platform", 2, "DEPLOYMENT_RUN_STATUS_FAILED")
	if !strings.Contains(failed["error"].(string), "secrets/prod") {
		t.Errorf("the failure: %v", failed["error"])
	}
	statuses := map[string]any{}
	for _, s := range asList(failed["steps"]) {
		s := s.(map[string]any)
		statuses[s["workspace"].(map[string]any)["project"].(string)] = s["runStatus"]
	}
	if statuses["secrets"] != "RUN_STATUS_FAILED" || statuses["eks"] != "RUN_STATUS_CANCELED" {
		t.Errorf("steps: %v", statuses)
	}
	kms := w.runsOf("kms", "prod")
	if kms[0]["status"] == "RUN_STATUS_FAILED" || kms[0]["status"] == "RUN_STATUS_CANCELED" {
		t.Errorf("kms's newest run: %v", kms[0]["status"])
	}
	var kmsApplied int
	if err := w.world.owner.QueryRow(context.Background(), `SELECT count(*) FROM runs r JOIN workspaces w ON w.id = r.workspace_id
		JOIN projects p ON p.id = w.project_id WHERE p.slug = 'kms' AND r.status = 'applied'`).Scan(&kmsApplied); err != nil || kmsApplied != 1 {
		t.Errorf("kms applied %d times: %v", kmsApplied, err)
	}
}

func TestDriftIsFoundAndReverted(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.project(w.owner, "a", "drifty", "prod")
	ws := map[string]any{"org": "a", "project": "drifty", "workspace": "prod"}
	if _, code := w.owner.call("RunService/CreateRun", merge(ws, map[string]any{"checkDrift": true})); code != "failed_precondition" {
		t.Errorf("a drift check before any apply: %q", code)
	}
	w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "drifty")),
		"sourceName": "drifty.tar.gz"}))
	w.waitFor(ws, 1, "RUN_STATUS_NEEDS_APPROVAL")
	w.reviewer.must("RunService/ApproveRun", merge(ws, map[string]any{"number": 1}))
	w.waitFor(ws, 1, "RUN_STATUS_APPLIED")

	if err := w.runs.QueueDrift(context.Background(), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	check := w.waitFor(ws, 2, "RUN_STATUS_PLANNED")
	if check["trigger"] != "drift" || check["planOnly"] != true {
		t.Errorf("the drift run: %v", check)
	}
	drift := withCheck(findingsOf(w.viewer, ws, "SecurityService/ListFindings"), "DRIFT")
	if len(drift) != 1 || drift[0]["resource"] != "local_file.marker" || drift[0]["severity"] != "SEVERITY_MEDIUM" {
		t.Fatalf("drift findings: %v", drift)
	}

	w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"revert": true}))
	revert := w.waitFor(ws, 3, "RUN_STATUS_NEEDS_APPROVAL")
	if revert["source"] != "drifty.tar.gz" {
		t.Errorf("the revert ran %v", revert["source"])
	}
}

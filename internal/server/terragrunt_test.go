package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"
)

// unitsStarted lists the units a run's job started, in order.
func (w *runWorld) unitsStarted(runID, job string) []string {
	w.t.Helper()
	rows, err := w.world.owner.Query(context.Background(), `SELECT e.payload FROM job_events e JOIN runs r ON e.job_id = r.`+job+`
		WHERE r.id = $1 AND e.type = 'executor.unit_started' ORDER BY e.seq`, runID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var units []string
	for rows.Next() {
		var p map[string]any
		if err := rows.Scan(&p); err != nil {
			w.t.Fatal(err)
		}
		units = append(units, p["unit"].(string))
	}
	return units
}

func TestTerragruntStackPlansInOrderAndApplies(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.owner.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": "stack", "path": "live", "engine": "ENGINE_OPENTOFU",
		"engineVersion": "1.10.7", "layout": "LAYOUT_PLAIN"})
	if _, err := w.world.owner.Exec(context.Background(), `UPDATE projects SET layout = 'terragrunt', state_backend = 'code' WHERE slug = 'stack'`); err != nil {
		t.Fatal(err)
	}
	ws := map[string]any{"org": "a", "project": "stack", "workspace": "default"}
	run := w.owner.must("RunService/CreateRun", merge(ws, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, "terragrunt")),
		"sourceName": "terragrunt.tar.gz"}))["run"].(map[string]any)
	w.waitFor(ws, 1, "RUN_STATUS_NEEDS_APPROVAL")

	planned := w.unitsStarted(run["id"].(string), "plan_job_id")
	if len(planned) != 3 || planned[0] != "vpc" || !slices.Contains(planned, "app") || !slices.Contains(planned, "db") {
		t.Errorf("planned units %v, want vpc first", planned)
	}
	g := w.owner.must("GraphService/GetPlanGraph", merge(ws, map[string]any{"number": 1}))["graph"].(map[string]any)
	var groups, nodes []string
	for _, gr := range asList(g["groups"]) {
		groups = append(groups, gr.(map[string]any)["id"].(string))
	}
	for _, n := range asList(g["nodes"]) {
		nodes = append(nodes, n.(map[string]any)["id"].(string))
	}
	slices.Sort(groups)
	slices.Sort(nodes)
	if !slices.Equal(groups, []string{"module.app", "module.db", "module.vpc"}) ||
		!slices.Equal(nodes, []string{"module.app.terraform_data.this", "module.db.terraform_data.this", "module.vpc.terraform_data.this"}) {
		t.Errorf("groups %v, nodes %v", groups, nodes)
	}

	w.reviewer.must("RunService/ApproveRun", merge(ws, map[string]any{"number": 1}))
	w.waitFor(ws, 1, "RUN_STATUS_APPLIED")
	applied := w.unitsStarted(run["id"].(string), "apply_job_id")
	if len(applied) != 3 || applied[0] != "vpc" {
		t.Errorf("applied units %v, want vpc first", applied)
	}
	rows, err := w.world.owner.Query(context.Background(), `SELECT e.payload FROM job_events e JOIN runs r ON e.job_id = r.apply_job_id
		WHERE r.id = $1 AND e.type = 'engine.apply_complete'`, run["id"])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var done []string
	for rows.Next() {
		var p json.RawMessage
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		done = append(done, string(p))
	}
	if len(done) != 3 {
		t.Errorf("%d resources applied, want 3: %v", len(done), done)
	}
}

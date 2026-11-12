package server_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stackwright/stackwright/fixtures"
	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/graph"
	"github.com/stackwright/stackwright/internal/jobs"
)

var update = flag.Bool("update", false, "write the plan graph golden files from real plans")

type graphCase struct {
	name, engine, version, path string
	// sources are applied in order; the last one is only planned.
	sources []func(t *testing.T) []byte
}

func fixture(name string) func(t *testing.T) []byte {
	return func(t *testing.T) []byte { return source(t, filepath.Join("..", "graph", name)) }
}

func TestPlanGraphGolden(t *testing.T) {
	t.Parallel()
	w := newJobWorld(t)
	cases := []graphCase{
		{name: "sample", engine: "ENGINE_OPENTOFU", version: "1.10.7", path: "sample", sources: []func(*testing.T) []byte{
			func(t *testing.T) []byte {
				b, err := fixtures.Sample()
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
		}},
		{name: "actions", engine: "ENGINE_OPENTOFU", version: "1.10.7", sources: []func(*testing.T) []byte{fixture("actions/v1"), fixture("actions/v2")}},
		{name: "actions", engine: "ENGINE_TERRAFORM", version: "1.5.7", sources: []func(*testing.T) []byte{fixture("actions/v1"), fixture("actions/v2")}},
		{name: "modules", engine: "ENGINE_OPENTOFU", version: "1.10.7", path: "live", sources: []func(*testing.T) []byte{fixture("modules")}},
	}
	for i, c := range cases {
		t.Run(c.name+"/"+c.engine, func(t *testing.T) {
			slug := c.name + "-" + string(rune('a'+i))
			w.api.must("ProjectService/CreateProject", map[string]any{"org": "a", "slug": slug, "path": c.path, "engine": c.engine,
				"engineVersion": c.version, "layout": "LAYOUT_PLAIN"})
			orgID, ws := w.workspaceID(slug, "default")
			var planID string
			for n, src := range c.sources {
				id, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_PLAN, Source: src(t)})
				if code != 0 {
					t.Fatalf("plan exited %d:\n%s", code, logs)
				}
				planID = id
				if n == len(c.sources)-1 {
					break
				}
				if _, code, logs := w.run(jobs.Request{OrgID: orgID, WorkspaceID: ws, Operation: v1.JobOperation_JOB_OPERATION_APPLY, PlanJobID: id}); code != 0 {
					t.Fatalf("apply exited %d:\n%s", code, logs)
				}
			}
			plan, sources := w.artifact(planID, "plan.json"), w.artifact(planID, "sources.json")
			g, err := graph.Build(plan, sources)
			if err != nil {
				t.Fatal(err)
			}
			got := golden(t, g)
			dir := filepath.Join("..", "graph", "testdata", c.name)
			if *update && c.engine == "ENGINE_OPENTOFU" {
				for name, b := range map[string][]byte{"plan.json": plan, "sources.json": sources, "graph.json": got} {
					if err := os.MkdirAll(dir, 0o750); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			want, err := os.ReadFile(filepath.Join(dir, "graph.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("graph differs from %s/graph.json; run with -update to see the change:\n%s", dir, got)
			}
		})
	}
}

// golden writes g as indented JSON. protojson varies its spacing on
// purpose, so its output goes through encoding/json.
func golden(t *testing.T, g *v1.PlanGraph) []byte {
	t.Helper()
	b, err := protojson.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestPlanGraphAPI(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	if _, code := w.viewer.call("GraphService/GetPlanGraph", dev(nil)); code != "not_found" {
		t.Errorf("a workspace with no plan: %q", code)
	}
	w.owner.must("RunService/CreateRun", dev(map[string]any{"planOnly": true}))
	w.waitRun(1, "RUN_STATUS_PLANNED")

	out := w.viewer.must("GraphService/GetPlanGraph", dev(nil))
	g := out["graph"].(map[string]any)
	node := asList(g["nodes"])[0].(map[string]any)
	if out["number"] != float64(1) || node["id"] != "terraform_data.greeting" || node["action"] != "PLAN_ACTION_CREATE" ||
		node["source"].(map[string]any)["file"] != "sample/main.tf" {
		t.Errorf("graph: %v", out)
	}
	if out := w.viewer.must("GraphService/GetPlanGraph", dev(map[string]any{"actions": []string{"PLAN_ACTION_DELETE"}})); len(asList(out["graph"].(map[string]any)["nodes"])) != 0 {
		t.Errorf("a delete filter kept nodes: %v", out)
	}

	detail := w.viewer.must("GraphService/GetPlanNode", dev(map[string]any{"number": 1, "id": "terraform_data.greeting"}))
	var input map[string]any
	for _, c := range asList(asList(detail["instances"])[0].(map[string]any)["changes"]) {
		if c.(map[string]any)["path"] == "input" {
			input = c.(map[string]any)
		}
	}
	if input["after"] != `"hello"` {
		t.Errorf("the input's change: %v", detail)
	}
	if _, code := w.viewer.call("GraphService/GetPlanNode", dev(map[string]any{"number": 1, "id": "terraform_data.nothing"})); code != "not_found" {
		t.Errorf("an unknown node: %q", code)
	}

	save := func(c *client, replace bool, positions ...map[string]any) string {
		_, code := c.call("GraphService/SaveGraphLayout", dev(map[string]any{"positions": positions, "replace": replace}))
		return code
	}
	at := func(id string, x, y float64) map[string]any { return map[string]any{"id": id, "x": x, "y": y} }
	if code := save(w.viewer, false, at("terraform_data.greeting", 1, 2)); code != "permission_denied" {
		t.Errorf("a viewer saved a layout: %q", code)
	}
	if code := save(w.owner, false, at("terraform_data.greeting", 1, 2), at("gone.node", 5, 5)); code != "" {
		t.Fatalf("save: %q", code)
	}
	if code := save(w.owner, false, at("terraform_data.greeting", 10, 20)); code != "" {
		t.Fatalf("merge: %q", code)
	}
	layout := asList(w.viewer.must("GraphService/GetPlanGraph", dev(nil))["layout"])
	if len(layout) != 1 || layout[0].(map[string]any)["x"] != float64(10) || layout[0].(map[string]any)["y"] != float64(20) {
		t.Errorf("layout after a merge: %v", layout)
	}
	if code := save(w.owner, true, at("other.node", 1, 1)); code != "" {
		t.Fatalf("replace: %q", code)
	}
	if layout := asList(w.viewer.must("GraphService/GetPlanGraph", dev(nil))["layout"]); len(layout) != 0 {
		t.Errorf("a replace kept the old position: %v", layout)
	}
	if code := save(w.owner, false, map[string]any{"id": "", "x": 1, "y": 1}); code != "invalid_argument" {
		t.Errorf("an empty node id: %q", code)
	}
}

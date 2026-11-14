package executor

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/findings"
	"github.com/stackwright/stackwright/internal/graph"
)

// The units' plans are Terragrunt 1.2.1 and OpenTofu 1.10.7 output for a
// stack where app and db depend on vpc and plan with its mock outputs.
func TestMergedPlanHasEachUnitAsAModule(t *testing.T) {
	var units []unitPlan
	for _, u := range []string{"vpc", "app", "db"} {
		b, err := os.ReadFile("testdata/terragrunt/" + u + ".json")
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, unitPlan{slug: u, path: u, plan: b})
	}
	merged, err := mergePlans(units)
	if err != nil {
		t.Fatal(err)
	}

	g, err := graph.Build(merged, nil)
	if err != nil {
		t.Fatal(err)
	}
	var groups, nodes []string
	for _, gr := range g.Groups {
		groups = append(groups, gr.Id)
	}
	for _, n := range g.Nodes {
		nodes = append(nodes, n.Id)
		if n.Action != v1.PlanAction_PLAN_ACTION_CREATE {
			t.Errorf("%s: %v", n.Id, n.Action)
		}
	}
	slices.Sort(groups)
	slices.Sort(nodes)
	if !slices.Equal(groups, []string{"module.app", "module.db", "module.vpc"}) ||
		!slices.Equal(nodes, []string{"module.app.terraform_data.this", "module.db.terraform_data.this", "module.vpc.terraform_data.this"}) {
		t.Errorf("groups %v, nodes %v", groups, nodes)
	}

	_, changed, err := findings.Match(nil, merged)
	if err != nil {
		t.Fatal(err)
	}
	if !changed["module.app.terraform_data.this"] || len(changed) != 3 {
		t.Errorf("changed %v", changed)
	}

	var p struct {
		PlannedValues struct {
			RootModule struct {
				ChildModules []struct {
					Address   string
					Resources []struct{ Address string }
				} `json:"child_modules"`
			} `json:"root_module"`
		} `json:"planned_values"`
	}
	if err := json.Unmarshal(merged, &p); err != nil {
		t.Fatal(err)
	}
	if mods := p.PlannedValues.RootModule.ChildModules; len(mods) != 3 || mods[1].Address != "module.app" ||
		mods[1].Resources[0].Address != "module.app.terraform_data.this" {
		t.Errorf("planned values: %+v", mods)
	}
}

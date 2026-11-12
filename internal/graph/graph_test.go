package graph

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

var update = flag.Bool("update", false, "write graph.json from the stored plans")

func read(t *testing.T, name, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name, file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGolden(t *testing.T) {
	dirs, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		t.Run(d.Name(), func(t *testing.T) {
			g, err := Build(read(t, d.Name(), "plan.json"), read(t, d.Name(), "sources.json"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := protojson.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			var v any
			_ = json.Unmarshal(b, &v)
			got, _ := json.MarshalIndent(v, "", "  ")
			got = append(got, '\n')
			if *update {
				if err := os.WriteFile(filepath.Join("testdata", d.Name(), "graph.json"), got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			if want := read(t, d.Name(), "graph.json"); !bytes.Equal(got, want) {
				t.Errorf("graph differs from graph.json:\n%s", got)
			}
		})
	}
}

func TestNodeDiffs(t *testing.T) {
	plan, sources := read(t, "actions", "plan.json"), read(t, "actions", "sources.json")
	change := func(id, path string) *v1.AttributeChange {
		t.Helper()
		n, err := Node(plan, sources, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range n.Instances[0].Changes {
			if c.Path == path {
				return c
			}
		}
		t.Fatalf("%s has no change at %s: %v", id, path, n.Instances[0].Changes)
		return nil
	}
	for _, tc := range []struct {
		id, path, before, after string
		forces                  bool
	}{
		{"terraform_data.change", "input", `"a"`, `"b"`, false},
		{"terraform_data.change", "output", `"a"`, unknown, false},
		{"terraform_data.secret", "input", sensitive, sensitive, false},
		{"terraform_data.recreate", "triggers_replace", `"a"`, `"b"`, true},
		{"terraform_data.added", "id", "", unknown, false},
		{"terraform_data.gone", "input", `"bye"`, "", false},
	} {
		c := change(tc.id, tc.path)
		if c.Before != tc.before || c.After != tc.after || c.ForcesReplacement != tc.forces {
			t.Errorf("%s %s: %q -> %q forces=%v, want %q -> %q forces=%v", tc.id, tc.path, c.Before, c.After, c.ForcesReplacement,
				tc.before, tc.after, tc.forces)
		}
	}
	added, err := Node(plan, sources, "terraform_data.added")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range added.Instances[0].Changes {
		if c.After == "null" {
			t.Errorf("a create lists the null attribute %s", c.Path)
		}
	}
	n, err := Node(plan, sources, "terraform_data.keep")
	if err != nil || len(n.Instances) != 1 || len(n.Instances[0].Changes) != 0 {
		t.Errorf("a no-op lists changes: %v, %v", n, err)
	}
	if _, err := Node(plan, sources, "terraform_data.nothing"); err == nil {
		t.Error("an unknown node was found")
	}
}

func TestFilter(t *testing.T) {
	g, err := Build(read(t, "modules", "plan.json"), read(t, "modules", "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := Filter(g, nil, "module.net")
	for _, n := range f.Nodes {
		if n.Module != "module.net" && n.Module != "module.net.module.sub" {
			t.Errorf("module filter kept %s", n.Id)
		}
	}
	if len(f.Nodes) != 2 || len(f.Groups) != 2 || len(f.Edges) != 1 {
		t.Errorf("module.net: %d nodes, %d groups, %d edges", len(f.Nodes), len(f.Groups), len(f.Edges))
	}
	reads := Filter(g, []v1.PlanAction{v1.PlanAction_PLAN_ACTION_READ}, "")
	if len(reads.Nodes) != 1 || reads.Nodes[0].Id != "module.app.data.local_file.config" || len(reads.Groups) != 1 {
		t.Errorf("reads: %v", reads)
	}
}

// The web app strips keys too, and its test reads the same cases.
func TestStripKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ In, Want string }
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := stripKeys(c.In); got != c.Want {
			t.Errorf("%s: %s, want %s", c.In, got, c.Want)
		}
	}
}

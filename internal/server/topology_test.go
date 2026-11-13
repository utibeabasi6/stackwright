package server_test

import (
	"encoding/base64"
	"testing"
)

func TestTopologyReadsTheLatestPlan(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t)
	w.project(w.owner, "a", "edge", "prod")
	prod := map[string]any{"org": "a", "project": "edge", "workspace": "prod"}
	plan := func(fixture string, number int) {
		w.owner.must("RunService/CreateRun", merge(prod, map[string]any{"source": base64.StdEncoding.EncodeToString(source(t, fixture)),
			"sourceName": fixture + ".tar.gz", "planOnly": true}))
		w.waitFor(prod, number, "RUN_STATUS_PLANNED")
	}
	group := func() (map[string]any, []any) {
		out := w.viewer.must("GraphService/GetTopology", map[string]any{"org": "a"})
		topo, _ := out["topology"].(map[string]any)
		for _, n := range asList(topo["nodes"]) {
			if n := n.(map[string]any); n["id"] == "edge/prod/aws_security_group.ssh" {
				return n, asList(topo["edges"])
			}
		}
		t.Fatalf("no security group in %v", out)
		return nil, nil
	}

	plan("ssh-open", 1)
	n, edges := group()
	if n["findings"] != float64(1) || n["worst"] != "SEVERITY_HIGH" || n["action"] != "PLAN_ACTION_CREATE" {
		t.Errorf("the open group: %v", n)
	}
	if len(edges) != 1 || edges[0].(map[string]any)["from"] != "internet" || edges[0].(map[string]any)["label"] != "tcp 22" {
		t.Errorf("edges: %v", edges)
	}

	plan("ssh-closed", 2)
	n, edges = group()
	rules := asList(n["rules"])
	if _, ok := n["findings"]; ok || len(edges) != 0 || len(rules) != 1 || asList(rules[0].(map[string]any)["cidrs"])[0] != "10.0.0.0/8" {
		t.Errorf("after the fix: %v, edges %v", n, edges)
	}
}

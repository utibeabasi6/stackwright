package executor

import (
	"encoding/json"
	"fmt"
)

// unitPlan is one Terragrunt unit's `show -json` output.
type unitPlan struct {
	slug, path string
	plan       []byte
}

// mergePlans makes one plan of a stack's units, with each unit as a call
// of the root module named by its slug, so the graph, the security scan
// and the topology read a stack as they read one root (adr/0041).
// https://opentofu.org/docs/internals/json-format/
func mergePlans(units []unitPlan) ([]byte, error) {
	out := map[string]any{}
	var changes []any
	planned := map[string]any{"child_modules": []any{}}
	prior := map[string]any{"child_modules": []any{}}
	calls := map[string]any{}
	for _, u := range units {
		var p map[string]any
		if err := json.Unmarshal(u.plan, &p); err != nil {
			return nil, fmt.Errorf("unit %s: the plan is not JSON: %w", u.path, err)
		}
		if out["format_version"] == nil {
			out["format_version"], out["terraform_version"] = p["format_version"], p["terraform_version"]
		}
		prefix := "module." + u.slug
		for _, c := range list(p["resource_changes"]) {
			rc, _ := c.(map[string]any)
			if rc == nil {
				continue
			}
			rc["address"] = prefix + "." + str(rc["address"])
			if m := str(rc["module_address"]); m != "" {
				rc["module_address"] = prefix + "." + m
			} else {
				rc["module_address"] = prefix
			}
			changes = append(changes, rc)
		}
		if root := values(p["planned_values"]); root != nil {
			planned["child_modules"] = append(list(planned["child_modules"]), nest(root, prefix))
		}
		if root := values(object(p["prior_state"])["values"]); root != nil {
			prior["child_modules"] = append(list(prior["child_modules"]), nest(root, prefix))
		}
		config, _ := object(p["configuration"])["root_module"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		calls[u.slug] = map[string]any{"source": u.path, "module": config}
	}
	out["resource_changes"] = changes
	out["planned_values"] = map[string]any{"root_module": planned}
	out["prior_state"] = map[string]any{"values": map[string]any{"root_module": prior}}
	out["configuration"] = map[string]any{"root_module": map[string]any{"module_calls": calls}}
	return json.Marshal(out)
}

// values returns a values representation's root_module.
func values(v any) map[string]any {
	root, _ := object(v)["root_module"].(map[string]any)
	return root
}

// nest moves a module, and every module and resource in it, under prefix.
func nest(m map[string]any, prefix string) map[string]any {
	if a := str(m["address"]); a != "" {
		m["address"] = prefix + "." + a
	} else {
		m["address"] = prefix
	}
	for _, r := range list(m["resources"]) {
		if res, ok := r.(map[string]any); ok {
			res["address"] = prefix + "." + str(res["address"])
		}
	}
	for _, c := range list(m["child_modules"]) {
		if child, ok := c.(map[string]any); ok {
			nest(child, prefix)
		}
	}
	return m
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

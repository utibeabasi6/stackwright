package graph

import (
	"encoding/json"
	"fmt"
	"slices"
)

// stateModule is a module in a plan's prior_state, which carries each
// resource's sensitive_values from its provider's schema as well as from
// the configuration. The state file lists only the second.
// https://opentofu.org/docs/internals/json-format/#values-representation
type stateModule struct {
	Resources []struct {
		Address         string          `json:"address"`
		Values          json.RawMessage `json:"values"`
		SensitiveValues json.RawMessage `json:"sensitive_values"`
	} `json:"resources"`
	ChildModules []stateModule `json:"child_modules"`
}

// StateResource returns the attributes of a resource instance in the state
// that the plan read, as sorted "path = value" lines. Sensitive values show
// as "(sensitive)".
func StateResource(planJSON []byte, address string) ([]string, error) {
	var p struct {
		PriorState struct {
			Values struct {
				RootModule stateModule `json:"root_module"`
			} `json:"values"`
		} `json:"prior_state"`
	}
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	modules := []stateModule{p.PriorState.Values.RootModule}
	for len(modules) > 0 {
		m := modules[0]
		modules = append(modules[1:], m.ChildModules...)
		for _, r := range m.Resources {
			if r.Address != address {
				continue
			}
			secret := marks(r.SensitiveValues)
			var out []string
			for path, v := range leaves(r.Values) {
				out = append(out, path+" = "+value(v, under(path, secret)))
			}
			slices.Sort(out)
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoNode, address)
}

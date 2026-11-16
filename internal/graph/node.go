package graph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

const (
	maxChanges = 1000
	sensitive  = "(sensitive)"
	unknown    = "(known after apply)"
)

// Node returns one node and its instances, each with the attributes that
// change. A create lists every attribute it sets; a delete, every one it
// had. Null attributes are left out of both.
func Node(planJSON, sourcesJSON []byte, id string) (*v1.GetPlanNodeResponse, error) {
	b, err := parse(planJSON, sourcesJSON)
	if err != nil {
		return nil, err
	}
	n, ok := b.nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoNode, id)
	}
	out := &v1.GetPlanNodeResponse{Node: n}
	for _, rc := range b.changes[id] {
		in := &v1.PlanInstance{Address: rc.Address, Action: Action(rc.Change.Actions)}
		if in.Action != v1.PlanAction_PLAN_ACTION_NO_OP {
			in.Changes, in.Truncated = diff(rc.Change)
		}
		out.Instances = append(out.Instances, in)
	}
	return out, nil
}

func diff(c change) ([]*v1.AttributeChange, bool) {
	before, after := leaves(c.Before), leaves(c.After)
	unknowns := marks(c.AfterUnknown)
	beforeSecret, afterSecret := marks(c.BeforeSensitive), marks(c.AfterSensitive)
	var forcing []string
	for _, p := range c.ReplacePaths {
		forcing = append(forcing, render(p))
	}

	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	for _, p := range unknowns {
		paths[p] = true
	}
	var out []*v1.AttributeChange
	for p := range paths {
		b, hadBefore := before[p]
		a, hasAfter := after[p]
		isUnknown := under(p, unknowns)
		if hadBefore == hasAfter && !isUnknown && value(b, false) == value(a, false) {
			continue
		}
		// As the engines' own plan output does, a create or a delete leaves out
		// attributes that are null.
		if !isUnknown && (!hadBefore && a == nil || !hasAfter && b == nil) {
			continue
		}
		ac := &v1.AttributeChange{Path: p}
		if hadBefore {
			ac.Before = value(b, under(p, beforeSecret))
		}
		switch {
		case isUnknown:
			ac.After = unknown
		case hasAfter:
			ac.After = value(a, under(p, afterSecret))
		}
		ac.ForcesReplacement = under(p, forcing)
		out = append(out, ac)
	}
	slices.SortFunc(out, func(x, y *v1.AttributeChange) int { return strings.Compare(x.Path, y.Path) })
	if len(out) > maxChanges {
		return out[:maxChanges], true
	}
	return out, false
}

// value renders a JSON value, or the sensitive mark. The plan's JSON holds
// sensitive values too, so a change is found before they are masked.
func value(v any, secret bool) string {
	if secret {
		return sensitive
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// leaves flattens a JSON value to its leaves, keyed by path. An empty object
// or list is a leaf.
func leaves(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return out
	}
	var walk func(path []any, v any)
	walk = func(path []any, v any) {
		switch v := v.(type) {
		case map[string]any:
			if len(v) == 0 && len(path) > 0 {
				out[render(path)] = v
			}
			for k, x := range v {
				walk(append(slices.Clone(path), k), x)
			}
		case []any:
			if len(v) == 0 && len(path) > 0 {
				out[render(path)] = v
			}
			for i, x := range v {
				walk(append(slices.Clone(path), float64(i)), x)
			}
		default:
			if len(path) > 0 {
				out[render(path)] = v
			}
		}
	}
	walk(nil, v)
	return out
}

// marks returns the paths where a mask such as after_unknown is true. A
// true at a path covers everything under it.
func marks(raw json.RawMessage) []string {
	var out []string
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var walk func(path []any, v any)
	walk = func(path []any, v any) {
		switch v := v.(type) {
		case bool:
			if v {
				out = append(out, render(path))
			}
		case map[string]any:
			for k, x := range v {
				walk(append(slices.Clone(path), k), x)
			}
		case []any:
			for i, x := range v {
				walk(append(slices.Clone(path), float64(i)), x)
			}
		}
	}
	walk(nil, v)
	return out
}

// under reports whether path is one of prefixes, or below one. The empty
// prefix covers every path.
func under(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" || path == p || strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[") {
			return true
		}
	}
	return false
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// render writes a path as `tags.Name`, `ingress[0].from_port` or
// `tags["kubernetes.io/role"]`.
func render(path []any) string {
	var s strings.Builder
	for _, p := range path {
		switch p := p.(type) {
		case float64:
			fmt.Fprintf(&s, "[%d]", int(p))
		case string:
			if !identifier.MatchString(p) {
				fmt.Fprintf(&s, "[%q]", p)
				continue
			}
			if s.Len() > 0 {
				s.WriteByte('.')
			}
			s.WriteString(p)
		}
	}
	return s.String()
}

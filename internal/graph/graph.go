// Package graph builds the plan graph from a plan's JSON and the sources
// map that the executor writes. ADR 0027 describes the model.
package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
)

// ErrNoNode is returned for a node id that the plan does not have.
var ErrNoNode = errors.New("the plan has no such node")

// plan holds the parts of `show -json` that the graph reads.
// https://opentofu.org/docs/internals/json-format/
type plan struct {
	ResourceChanges []resourceChange `json:"resource_changes"`
	Configuration   struct {
		RootModule configModule `json:"root_module"`
	} `json:"configuration"`
}

type resourceChange struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address"`
	Mode          string `json:"mode"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	Change        change `json:"change"`
}

type change struct {
	Actions         []string        `json:"actions"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	AfterUnknown    json.RawMessage `json:"after_unknown"`
	BeforeSensitive json.RawMessage `json:"before_sensitive"`
	AfterSensitive  json.RawMessage `json:"after_sensitive"`
	ReplacePaths    [][]any         `json:"replace_paths"`
}

type configModule struct {
	Resources   []configResource      `json:"resources"`
	ModuleCalls map[string]moduleCall `json:"module_calls"`
	Outputs     map[string]struct {
		Expression json.RawMessage `json:"expression"`
	} `json:"outputs"`
}

type configResource struct {
	Address           string          `json:"address"`
	Mode              string          `json:"mode"`
	Type              string          `json:"type"`
	Name              string          `json:"name"`
	Expressions       json.RawMessage `json:"expressions"`
	CountExpression   json.RawMessage `json:"count_expression"`
	ForEachExpression json.RawMessage `json:"for_each_expression"`
	DependsOn         []string        `json:"depends_on"`
}

type moduleCall struct {
	Expressions       map[string]json.RawMessage `json:"expressions"`
	CountExpression   json.RawMessage            `json:"count_expression"`
	ForEachExpression json.RawMessage            `json:"for_each_expression"`
	DependsOn         []string                   `json:"depends_on"`
	Module            configModule               `json:"module"`
}

// Sources is the map that the executor uploads as sources.json, keyed by
// module id ("" for the root module).
type Sources struct {
	Modules map[string]SourceModule `json:"modules"`
}

type SourceModule struct {
	// Keyed by address in the module, such as "aws_vpc.this".
	Resources map[string]Location `json:"resources,omitempty"`
	// Keyed by the call's name.
	ModuleCalls map[string]Location `json:"module_calls,omitempty"`
	// Each local's references, such as "aws_vpc.this.id".
	Locals map[string][]string `json:"locals,omitempty"`
	Remote bool                `json:"remote,omitempty"`
}

type Location struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type builder struct {
	modules map[string]*configModule
	sources Sources
	nodes   map[string]*v1.PlanNode
	changes map[string][]resourceChange
	edges   map[[2]string]bool
}

func parse(planJSON, sourcesJSON []byte) (*builder, error) {
	var p plan
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return nil, fmt.Errorf("plan.json: %w", err)
	}
	b := &builder{modules: map[string]*configModule{}, nodes: map[string]*v1.PlanNode{}, changes: map[string][]resourceChange{},
		edges: map[[2]string]bool{}}
	if len(sourcesJSON) > 0 {
		if err := json.Unmarshal(sourcesJSON, &b.sources); err != nil {
			return nil, fmt.Errorf("sources.json: %w", err)
		}
	}
	b.addModule("", &p.Configuration.RootModule)
	for _, rc := range p.ResourceChanges {
		module := StripKeys(rc.ModuleAddress)
		id := join(module, resourceAddress(rc.Mode, rc.Type, rc.Name))
		n := b.node(id, module, rc.Mode, rc.Type, rc.Name)
		n.Instances++
		b.changes[id] = append(b.changes[id], rc)
	}
	for _, n := range b.nodes {
		counts := map[v1.PlanAction]int32{}
		for _, rc := range b.changes[n.Id] {
			counts[Action(rc.Change.Actions)]++
		}
		n.Action = v1.PlanAction_PLAN_ACTION_NO_OP
		for a, c := range counts {
			n.Counts = append(n.Counts, &v1.ActionCount{Action: a, Count: c})
			n.Action = max(n.Action, a)
		}
		slices.SortFunc(n.Counts, func(x, y *v1.ActionCount) int { return int(x.Action - y.Action) })
	}
	return b, nil
}

func (b *builder) addModule(id string, m *configModule) {
	b.modules[id] = m
	for _, r := range m.Resources {
		b.node(join(id, r.Address), id, r.Mode, r.Type, r.Name)
	}
	for name, call := range m.ModuleCalls {
		b.addModule(join(id, "module."+name), &call.Module)
	}
}

func (b *builder) node(id, module, mode, typ, name string) *v1.PlanNode {
	if n, ok := b.nodes[id]; ok {
		return n
	}
	n := &v1.PlanNode{Id: id, Module: module, Type: typ, Name: name, Data: mode == "data"}
	address := strings.TrimPrefix(strings.TrimPrefix(id, module), ".")
	if loc, ok := b.sources.Modules[module].Resources[address]; ok {
		n.Source = &v1.SourceLocation{File: loc.File, Line: int32(loc.Line), Remote: b.sources.Modules[module].Remote} //nolint:gosec // a line number
	}
	b.nodes[id] = n
	return n
}

// Build returns the plan's graph. sourcesJSON may be empty.
func Build(planJSON, sourcesJSON []byte) (*v1.PlanGraph, error) {
	b, err := parse(planJSON, sourcesJSON)
	if err != nil {
		return nil, err
	}
	g := &v1.PlanGraph{Summary: &v1.PlanSummary{}}
	groups := map[string]bool{}
	for id := range b.modules {
		groups[id] = true
	}
	for _, n := range b.nodes {
		groups[n.Module] = true
		g.Nodes = append(g.Nodes, n)
		for _, rc := range b.changes[n.Id] {
			count(g.Summary, Action(rc.Change.Actions))
		}
	}
	for id := range groups {
		for ; id != ""; id = parentModule(id) {
			if !slices.ContainsFunc(g.Groups, func(x *v1.PlanGroup) bool { return x.Id == id }) {
				g.Groups = append(g.Groups, b.group(id))
			}
		}
	}

	for id, m := range b.modules {
		for _, r := range m.Resources {
			to := join(id, r.Address)
			for _, ref := range references(r.Expressions, r.CountExpression, r.ForEachExpression) {
				b.edge(id, ref, to, false)
			}
			for _, ref := range r.DependsOn {
				b.edge(id, ref, to, true)
			}
		}
		for name, call := range m.ModuleCalls {
			to := join(id, "module."+name)
			for _, ref := range references(call.CountExpression, call.ForEachExpression) {
				b.edge(id, ref, to, false)
			}
			for _, ref := range call.DependsOn {
				b.edge(id, ref, to, true)
			}
		}
	}
	for e, explicit := range b.edges {
		g.Edges = append(g.Edges, &v1.PlanEdge{From: e[0], To: e[1], Explicit: explicit})
	}

	slices.SortFunc(g.Nodes, func(x, y *v1.PlanNode) int { return strings.Compare(x.Id, y.Id) })
	slices.SortFunc(g.Groups, func(x, y *v1.PlanGroup) int { return strings.Compare(x.Id, y.Id) })
	slices.SortFunc(g.Edges, func(x, y *v1.PlanEdge) int {
		return strings.Compare(x.From+"\x00"+x.To, y.From+"\x00"+y.To)
	})
	return g, nil
}

func (b *builder) group(id string) *v1.PlanGroup {
	parent, name := parentModule(id), callName(id)
	g := &v1.PlanGroup{Id: id, Parent: parent, Name: name}
	if loc, ok := b.sources.Modules[parent].ModuleCalls[name]; ok {
		g.Source = &v1.SourceLocation{File: loc.File, Line: int32(loc.Line), Remote: b.sources.Modules[parent].Remote} //nolint:gosec // a line number
	}
	return g
}

// edge adds an edge from each thing that ref, read in module, leads to.
// An explicit edge stays explicit when a reference adds it too.
func (b *builder) edge(module, ref, to string, explicit bool) {
	for _, from := range b.resolve(module, ref, map[string]bool{}) {
		if from == to {
			continue
		}
		k := [2]string{from, to}
		b.edges[k] = b.edges[k] || explicit
	}
}

// resolve returns the nodes and groups that a reference in module leads to.
func (b *builder) resolve(module, ref string, seen map[string]bool) []string {
	key := module + "|" + ref
	if seen[key] {
		return nil
	}
	seen[key] = true
	parts := strings.Split(StripKeys(ref), ".")
	if len(parts) < 2 {
		return nil
	}
	var out []string
	follow := func(in string, refs []string) {
		for _, r := range refs {
			out = append(out, b.resolve(in, r, seen)...)
		}
	}
	switch parts[0] {
	case "each", "count", "path", "self", "terraform":
	case "var":
		parent := parentModule(module)
		if m := b.modules[parent]; module != "" && m != nil {
			follow(parent, references(m.ModuleCalls[callName(module)].Expressions[parts[1]]))
		}
	case "local":
		follow(module, b.sources.Modules[module].Locals[parts[1]])
	case "module":
		child := join(module, "module."+parts[1])
		m := b.modules[child]
		switch {
		case m == nil:
		case len(parts) == 2:
			out = append(out, child)
		default:
			follow(child, references(m.Outputs[parts[2]].Expression))
		}
	case "data":
		if len(parts) >= 3 {
			if id := join(module, "data."+parts[1]+"."+parts[2]); b.nodes[id] != nil {
				out = append(out, id)
			}
		}
	default:
		if id := join(module, parts[0]+"."+parts[1]); b.nodes[id] != nil {
			out = append(out, id)
		}
	}
	return out
}

// references collects every "references" list in expressions, at any depth:
// nested blocks hold their own expressions. The plan lists `module.app`
// beside `module.app.id`; the bare module is dropped then, so that only a
// reference to the whole module joins its group.
func references(exprs ...json.RawMessage) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if refs, ok := x.([]any); ok && k == "references" {
					for _, r := range refs {
						if s, ok := r.(string); ok {
							out = append(out, s)
						}
					}
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, e := range exprs {
		var v any
		if len(e) > 0 && json.Unmarshal(e, &v) == nil {
			walk(v)
		}
	}
	all := slices.Clone(out)
	return slices.DeleteFunc(out, func(ref string) bool {
		bare := StripKeys(ref)
		return strings.HasPrefix(bare, "module.") && strings.Count(bare, ".") == 1 &&
			slices.ContainsFunc(all, func(r string) bool { return strings.HasPrefix(StripKeys(r), bare+".") })
	})
}

// Action maps a plan change's actions to the API's action.
func Action(actions []string) v1.PlanAction {
	switch strings.Join(actions, ",") {
	case "no-op":
		return v1.PlanAction_PLAN_ACTION_NO_OP
	case "read":
		return v1.PlanAction_PLAN_ACTION_READ
	case "create":
		return v1.PlanAction_PLAN_ACTION_CREATE
	case "update":
		return v1.PlanAction_PLAN_ACTION_UPDATE
	case "delete,create":
		return v1.PlanAction_PLAN_ACTION_REPLACE
	case "create,delete":
		return v1.PlanAction_PLAN_ACTION_REPLACE_CREATE_FIRST
	case "delete":
		return v1.PlanAction_PLAN_ACTION_DELETE
	}
	return v1.PlanAction_PLAN_ACTION_UNSPECIFIED
}

func count(s *v1.PlanSummary, a v1.PlanAction) {
	switch a {
	case v1.PlanAction_PLAN_ACTION_CREATE:
		s.Creates++
	case v1.PlanAction_PLAN_ACTION_UPDATE:
		s.Updates++
	case v1.PlanAction_PLAN_ACTION_REPLACE, v1.PlanAction_PLAN_ACTION_REPLACE_CREATE_FIRST:
		s.Replaces++
	case v1.PlanAction_PLAN_ACTION_DELETE:
		s.Deletes++
	case v1.PlanAction_PLAN_ACTION_READ:
		s.Reads++
	case v1.PlanAction_PLAN_ACTION_NO_OP:
		s.Unchanged++
	}
}

func resourceAddress(mode, typ, name string) string {
	if mode == "data" {
		return "data." + typ + "." + name
	}
	return typ + "." + name
}

func join(module, address string) string {
	if module == "" {
		return address
	}
	return module + "." + address
}

// parentModule returns "module.a" for "module.a.module.b", and "" for
// "module.a".
func parentModule(id string) string {
	i := strings.LastIndex(id, ".module.")
	if i < 0 {
		return ""
	}
	return id[:i]
}

// callName returns "b" for "module.a.module.b".
func callName(id string) string {
	return id[strings.LastIndex(id, "module.")+len("module."):]
}

// StripKeys removes instance keys, which gives an address's node id:
// `module.a["x.y"].b[0]` becomes `module.a.b`. A key in quotes may hold
// dots and brackets.
func StripKeys(addr string) string {
	var out strings.Builder
	depth, quoted, escaped := 0, false, false
	for _, c := range addr {
		switch {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"' && depth > 0:
			quoted = !quoted
		case quoted:
		case c == '[':
			depth++
		case c == ']':
			depth--
		case depth == 0:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// Filter keeps the nodes with one of actions, or all of them when actions is
// empty, that sit in module or below it. It keeps the groups that still hold
// a node, and the edges whose two ends remain.
func Filter(g *v1.PlanGraph, actions []v1.PlanAction, module string) *v1.PlanGraph {
	if len(actions) == 0 && module == "" {
		return g
	}
	in := func(id string) bool { return module == "" || id == module || strings.HasPrefix(id, module+".") }
	out := &v1.PlanGraph{Summary: g.Summary}
	kept := map[string]bool{}
	for _, n := range g.Nodes {
		if in(n.Module) && (len(actions) == 0 || slices.Contains(actions, n.Action)) {
			out.Nodes = append(out.Nodes, n)
			kept[n.Id] = true
			for m := n.Module; m != ""; m = parentModule(m) {
				kept[m] = true
			}
		}
	}
	for _, gr := range g.Groups {
		if kept[gr.Id] {
			out.Groups = append(out.Groups, gr)
		}
	}
	for _, e := range g.Edges {
		if kept[e.From] && kept[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

package stacks

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// functions are the functions Stackwright offers where it evaluates: in
// deployment inputs and in for_each. The engine evaluates everything else
// with its own functions.
var functions = map[string]function.Function{
	"abs": stdlib.AbsoluteFunc, "can": tryfunc.CanFunc, "ceil": stdlib.CeilFunc, "chomp": stdlib.ChompFunc,
	"chunklist": stdlib.ChunklistFunc, "coalesce": stdlib.CoalesceFunc, "coalescelist": stdlib.CoalesceListFunc,
	"compact": stdlib.CompactFunc, "concat": stdlib.ConcatFunc, "contains": stdlib.ContainsFunc, "csvdecode": stdlib.CSVDecodeFunc,
	"distinct": stdlib.DistinctFunc, "element": stdlib.ElementFunc, "flatten": stdlib.FlattenFunc, "floor": stdlib.FloorFunc,
	"format": stdlib.FormatFunc, "formatlist": stdlib.FormatListFunc, "indent": stdlib.IndentFunc, "join": stdlib.JoinFunc,
	"jsondecode": stdlib.JSONDecodeFunc, "jsonencode": stdlib.JSONEncodeFunc, "keys": stdlib.KeysFunc, "length": stdlib.LengthFunc,
	"lookup": stdlib.LookupFunc, "lower": stdlib.LowerFunc, "max": stdlib.MaxFunc, "merge": stdlib.MergeFunc, "min": stdlib.MinFunc,
	"parseint": stdlib.ParseIntFunc, "range": stdlib.RangeFunc, "regex": stdlib.RegexFunc, "regexall": stdlib.RegexAllFunc,
	"replace": stdlib.ReplaceFunc, "reverse": stdlib.ReverseListFunc, "setintersection": stdlib.SetIntersectionFunc,
	"setproduct": stdlib.SetProductFunc, "setsubtract": stdlib.SetSubtractFunc, "setunion": stdlib.SetUnionFunc,
	"slice": stdlib.SliceFunc, "sort": stdlib.SortFunc, "split": stdlib.SplitFunc, "strrev": stdlib.ReverseFunc,
	"substr": stdlib.SubstrFunc, "title": stdlib.TitleFunc, "tobool": stdlib.MakeToFunc(cty.Bool),
	"tolist": stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)), "tomap": stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
	"tonumber": stdlib.MakeToFunc(cty.Number), "toset": stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
	"tostring": stdlib.MakeToFunc(cty.String), "trim": stdlib.TrimFunc, "trimprefix": stdlib.TrimPrefixFunc,
	"trimspace": stdlib.TrimSpaceFunc, "trimsuffix": stdlib.TrimSuffixFunc, "try": tryfunc.TryFunc, "upper": stdlib.UpperFunc,
	"values": stdlib.ValuesFunc, "zipmap": stdlib.ZipmapFunc,
}

// Env holds what a deployment's inputs read at run time. A sync has
// none of it, so those values are unknown then.
type Env struct {
	// Tokens are the identity tokens by block name.
	Tokens map[string]string
	// Store returns a workspace variable of a category.
	Store func(category, key string) (string, bool)
}

// Audiences returns each identity_token block's audience.
func (c *Config) Audiences() (map[string][]string, error) {
	out := map[string][]string{}
	for name, expr := range c.tokens {
		v, diags := expr.Value(&hcl.EvalContext{Functions: functions})
		if diags.HasErrors() {
			return nil, invalid(expr.Range(), "audience must be a list of strings: %s", diags.Error())
		}
		list, err := convert.Convert(v, cty.List(cty.String))
		if err != nil || !list.IsWhollyKnown() || list.IsNull() || list.LengthInt() == 0 {
			return nil, invalid(expr.Range(), "audience must be a list of strings")
		}
		for _, a := range list.AsValueSlice() {
			if a.IsNull() || !a.IsKnown() || !a.Type().Equals(cty.String) {
				return nil, invalid(expr.Range(), "audience must be a list of strings")
			}
			out[name] = append(out[name], a.AsString())
		}
	}
	return out, nil
}

// evalLocals evaluates locals in an order that their references allow. A
// local that fails, or that reads something unknown, is unknown.
func evalLocals(exprs map[string]hclsyntax.Expression, ctx *hcl.EvalContext, strict bool) (map[string]cty.Value, error) {
	out := map[string]cty.Value{}
	for len(out) < len(exprs) {
		progress := false
		for name, expr := range exprs {
			if _, done := out[name]; done {
				continue
			}
			ready := true
			for _, t := range expr.Variables() {
				attr, ok := hcl.TraverseAttr{}, false
				if len(t) > 1 {
					attr, ok = t[1].(hcl.TraverseAttr)
				}
				if t.RootName() == "local" && ok {
					dep := attr.Name
					_, done := out[dep]
					ready = ready && (done || exprs[dep] == nil)
				}
			}
			if !ready {
				continue
			}
			local := ctx.NewChild()
			local.Variables = map[string]cty.Value{"local": cty.ObjectVal(out)}
			v, diags := expr.Value(local)
			if diags.HasErrors() {
				if strict {
					return nil, invalid(expr.Range(), "local %q: %s", name, diags.Error())
				}
				v = cty.DynamicVal
			}
			out[name] = v
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("the locals refer to each other in a cycle")
		}
	}
	return out, nil
}

// Values evaluates a deployment's inputs and gives each stack variable
// its value, converted to its type.
func (c *Config) Values(deployment string, env Env) (map[string]cty.Value, error) {
	var d *Deployment
	for _, x := range c.Deployments {
		if x.Name == deployment {
			d = x
		}
	}
	if d == nil {
		return nil, fmt.Errorf("the Stack has no deployment %q", deployment)
	}
	tokens := map[string]cty.Value{}
	for name := range c.tokens {
		jwt := cty.UnknownVal(cty.String)
		if t, ok := env.Tokens[name]; ok {
			jwt = cty.StringVal(t)
		}
		tokens[name] = cty.ObjectVal(map[string]cty.Value{"jwt": jwt})
	}
	stores := map[string]cty.Value{}
	for name, category := range c.stores {
		stores[name] = cty.DynamicVal
		if env.Store == nil {
			continue
		}
		vals := map[string]cty.Value{}
		for _, key := range c.storeKeys(name) {
			if v, ok := env.Store(category, key); ok {
				vals[key] = cty.StringVal(v)
			}
		}
		obj := map[string]cty.Value{}
		for k, v := range vals {
			obj[k] = v
		}
		obj["stable"] = cty.ObjectVal(vals)
		stores[name] = cty.ObjectVal(obj)
	}
	ctx := &hcl.EvalContext{Functions: functions, Variables: map[string]cty.Value{
		"identity_token": cty.ObjectVal(tokens),
		"store":          cty.ObjectVal(map[string]cty.Value{"varset": cty.ObjectVal(stores)}),
	}}
	locals, err := evalLocals(c.deployLocals, ctx, true)
	if err != nil {
		return nil, err
	}
	ctx.Variables["local"] = cty.ObjectVal(locals)
	given := map[string]hcl.Range{}
	inputs := map[string]cty.Value{}
	if d.inputs != nil {
		for _, item := range d.inputs.Items {
			key := hcl.ExprAsKeyword(item.KeyExpr)
			if key == "" {
				v, diags := item.KeyExpr.Value(nil)
				if diags.HasErrors() || v.Type() != cty.String {
					return nil, invalid(item.KeyExpr.Range(), "an input name must be a variable name")
				}
				key = v.AsString()
			}
			v, diags := item.ValueExpr.Value(ctx)
			if diags.HasErrors() {
				return nil, invalid(item.ValueExpr.Range(), "input %q: %s", key, diags.Error())
			}
			inputs[key], given[key] = v, item.KeyExpr.Range()
		}
	}
	out := map[string]cty.Value{}
	for _, v := range c.Variables {
		ty, defaults, diags := typeexpr.TypeConstraintWithDefaults(v.block.Body.Attributes["type"].Expr)
		if diags.HasErrors() {
			return nil, invalid(v.block.TypeRange, "variable %q: %s", v.Name, diags.Error())
		}
		val, ok := inputs[v.Name]
		if !ok || val.IsNull() {
			a := v.block.Body.Attributes["default"]
			if a == nil {
				return nil, fmt.Errorf("deployment %q has no value for variable %q", d.Name, v.Name)
			}
			if val, err = literal(a); err != nil {
				return nil, err
			}
		}
		if defaults != nil {
			val = defaults.Apply(val)
		}
		if val, err = convert.Convert(val, ty); err != nil {
			return nil, fmt.Errorf("deployment %q, variable %q: %w", d.Name, v.Name, err)
		}
		out[v.Name] = val
	}
	for key, r := range given {
		if _, ok := out[key]; !ok {
			return nil, invalid(r, "deployment %q sets %q, which is not a stack variable", d.Name, key)
		}
	}
	return out, nil
}

// storeKeys lists the variables that the deployment files read from a
// store block.
func (c *Config) storeKeys(store string) []string {
	var keys []string
	add := func(e hclsyntax.Expression) {
		for _, t := range e.Variables() {
			var names []string
			for _, step := range t {
				switch s := step.(type) {
				case hcl.TraverseRoot:
					names = append(names, s.Name)
				case hcl.TraverseAttr:
					names = append(names, s.Name)
				}
			}
			if len(names) >= 4 && names[0] == "store" && names[1] == "varset" && names[2] == store {
				key := names[3]
				if key == "stable" && len(names) >= 5 {
					key = names[4]
				}
				if !slices.Contains(keys, key) {
					keys = append(keys, key)
				}
			}
		}
	}
	for _, d := range c.Deployments {
		if d.inputs != nil {
			add(d.inputs)
		}
	}
	for _, e := range c.deployLocals {
		add(e)
	}
	return keys
}

// stackContext is the context for for_each: the variables, and the stack
// locals that Stackwright can work out. Components are unknown.
func (c *Config) stackContext(vals map[string]cty.Value) *hcl.EvalContext {
	ctx := &hcl.EvalContext{Functions: functions, Variables: map[string]cty.Value{
		"var": cty.ObjectVal(vals), "component": cty.DynamicVal, "stack": cty.DynamicVal,
	}}
	locals, err := evalLocals(c.locals, ctx, false)
	if err != nil {
		locals = map[string]cty.Value{}
		for name := range c.locals {
			locals[name] = cty.DynamicVal
		}
	}
	ctx.Variables["local"] = cty.ObjectVal(locals)
	return ctx
}

// each is one instance of a for_each, as each.key and each.value.
type each struct {
	key   string
	value cty.Value
}

func (e each) object() cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"key": cty.StringVal(e.key), "value": e.value})
}

// forEach evaluates a for_each to its instances, by key.
func forEach(what string, expr hclsyntax.Expression, ctx *hcl.EvalContext) ([]each, error) {
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, invalid(expr.Range(), "the for_each of %s: %s", what, diags.Error())
	}
	if !v.IsWhollyKnown() {
		return nil, &Error{Range: expr.Range(), Msg: "a for_each that is not known before an apply, in " + what + ",", Err: ErrRefused}
	}
	if v.IsNull() {
		return nil, invalid(expr.Range(), "the for_each of %s is null", what)
	}
	var out []each
	ty := v.Type()
	switch {
	case ty.IsMapType() || ty.IsObjectType():
		for it := v.ElementIterator(); it.Next(); {
			k, val := it.Element()
			out = append(out, each{key: k.AsString(), value: val})
		}
	case ty.IsSetType() && ty.ElementType() == cty.String:
		for _, el := range v.AsValueSlice() {
			if el.IsNull() {
				return nil, invalid(expr.Range(), "the for_each of %s holds a null", what)
			}
			out = append(out, each{key: el.AsString(), value: el})
		}
	default:
		return nil, invalid(expr.Range(), "the for_each of %s must be a map or a set of strings", what)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// refs visits the references of an expression, and of the locals and
// provider configurations it reads, once each.
func (c *Config) refs(exprs []hclsyntax.Expression, seen map[string]bool, visit func(hcl.Traversal)) {
	for _, e := range exprs {
		if e == nil {
			continue
		}
		for _, t := range e.Variables() {
			visit(t)
			name := ""
			if len(t) > 1 {
				if a, ok := t[1].(hcl.TraverseAttr); ok {
					name = a.Name
				}
			}
			switch t.RootName() {
			case "local":
				if !seen["local."+name] && c.locals[name] != nil {
					seen["local."+name] = true
					c.refs([]hclsyntax.Expression{c.locals[name]}, seen, visit)
				}
			case "provider":
				if len(t) > 2 {
					attr, ok := t[2].(hcl.TraverseAttr)
					if !ok {
						continue
					}
					key := name + "." + attr.Name
					if p := c.providers[key]; p != nil && !seen["provider."+key] {
						seen["provider."+key] = true
						c.refs(append(bodyExprs(p.config), p.forEach), seen, visit)
					}
				}
			}
		}
	}
}

// bodyExprs lists every expression in a block, with its nested blocks.
func bodyExprs(b *hclsyntax.Block) []hclsyntax.Expression {
	if b == nil {
		return nil
	}
	var out []hclsyntax.Expression
	for _, a := range b.Body.Attributes {
		out = append(out, a.Expr)
	}
	for _, nb := range b.Body.Blocks {
		out = append(out, bodyExprs(nb)...)
	}
	return out
}

// needs returns the components that expressions read, through locals and
// provider configurations.
func (c *Config) needs(exprs []hclsyntax.Expression) []string {
	var out []string
	c.refs(exprs, map[string]bool{}, func(t hcl.Traversal) {
		if t.RootName() == "component" && len(t) > 1 {
			if a, ok := t[1].(hcl.TraverseAttr); ok && !slices.Contains(out, a.Name) {
				out = append(out, a.Name)
			}
		}
	})
	sort.Strings(out)
	return out
}

// Needs returns the components that a component reads or depends on.
func (c *Config) Needs(component string) []string {
	comp := c.Components[component]
	exprs := []hclsyntax.Expression{comp.forEach}
	if comp.inputs != nil {
		exprs = append(exprs, comp.inputs)
	}
	for _, p := range comp.providers {
		exprs = append(exprs, p.expr)
	}
	out := c.needs(exprs)
	for _, d := range comp.dependsOn {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// Keyed reports whether a component has for_each.
func (c *Config) Keyed(component string) bool {
	comp := c.Components[component]
	return comp != nil && comp.forEach != nil
}

// OutputsNeed returns the components that the stack outputs read.
func (c *Config) OutputsNeed() []string {
	var exprs []hclsyntax.Expression
	for _, o := range c.Outputs {
		exprs = append(exprs, o.value)
	}
	return c.needs(exprs)
}

// Instance is one component instance of a deployment, or the outputs of a
// deployment when Component is empty.
type Instance struct {
	Deployment string
	Component  string
	Key        string
	Keyed      bool
	// Removed is set for an instance of a removed block; Destroy says
	// whether it is destroyed or forgotten.
	Removed bool
	Destroy bool
	each    each
	removed *Removed
}

// Prior is an instance that a sync found before.
type Prior struct {
	Component string
	Key       string
	Keyed     bool
}

// Instances lists a deployment's instances: those of its components, those
// that removed blocks name from prior, and its outputs.
func (c *Config) Instances(deployment string, vals map[string]cty.Value, prior []Prior) ([]Instance, error) {
	ctx := c.stackContext(vals)
	var out []Instance
	names := make([]string, 0, len(c.Components))
	for name := range c.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		comp := c.Components[name]
		if comp.forEach == nil {
			out = append(out, Instance{Deployment: deployment, Component: name})
			continue
		}
		all, err := forEach("component "+name, comp.forEach, ctx)
		if err != nil {
			return nil, err
		}
		for _, e := range all {
			out = append(out, Instance{Deployment: deployment, Component: name, Key: e.key, Keyed: true, each: e})
		}
	}
	for _, r := range c.Removed {
		var keys []each
		switch {
		case r.forEach != nil:
			all, err := forEach("a removed block", r.forEach, ctx)
			if err != nil {
				return nil, err
			}
			for _, e := range all {
				local := ctx.NewChild()
				local.Variables = map[string]cty.Value{"each": e.object()}
				k, err := instanceKey(r.key, local)
				if err != nil {
					return nil, err
				}
				keys = append(keys, each{key: k, value: e.value})
			}
		case r.key != nil:
			k, err := instanceKey(r.key, ctx)
			if err != nil {
				return nil, err
			}
			keys = append(keys, each{key: k})
		}
		for _, p := range prior {
			if p.Component != r.Component {
				continue
			}
			named := r.key == nil || slices.ContainsFunc(keys, func(e each) bool { return e.key == p.Key })
			live := slices.ContainsFunc(out, func(i Instance) bool { return i.Component == p.Component && i.Key == p.Key && !i.Removed })
			if !named || live {
				continue
			}
			e := each{key: p.Key, value: cty.StringVal(p.Key)}
			for _, k := range keys {
				if k.key == p.Key && k.value != cty.NilVal {
					e = k
				}
			}
			out = append(out, Instance{Deployment: deployment, Component: p.Component, Key: p.Key, Keyed: p.Keyed,
				Removed: true, Destroy: r.Destroy, each: e, removed: r})
		}
	}
	for _, p := range prior {
		if !slices.ContainsFunc(out, func(i Instance) bool { return i.Component == p.Component && i.Key == p.Key }) {
			what := "component " + p.Component
			if p.Keyed {
				what += fmt.Sprintf("[%q]", p.Key)
			}
			return nil, fmt.Errorf("deployment %q: %s is gone from the files with no removed block, so its state would have no code", deployment, what)
		}
	}
	if len(c.Outputs) > 0 {
		out = append(out, Instance{Deployment: deployment})
	}
	return out, nil
}

func instanceKey(expr hclsyntax.Expression, ctx *hcl.EvalContext) (string, error) {
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", invalid(expr.Range(), "the instance key: %s", diags.Error())
	}
	if !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", invalid(expr.Range(), "the instance key must be a known string")
	}
	return v.AsString(), nil
}

var notWorkspace = regexp.MustCompile(`[^a-z0-9_-]+`)

// Workspace is the name of an instance's workspace.
func (i Instance) Workspace() string {
	parts := []string{i.Deployment, i.Component}
	switch {
	case i.Component == "":
		parts[1] = "outputs"
	case i.Keyed:
		parts = append(parts, i.Key)
	}
	name := strings.Trim(notWorkspace.ReplaceAllString(strings.ToLower(strings.Join(parts, "-")), "-"), "-_")
	return name
}

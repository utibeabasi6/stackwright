package stacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Module is what a generated root module needs from a component's module.
type Module struct {
	// Variables maps each variable to whether it has a default.
	Variables map[string]bool
	// Outputs maps each output to whether it is sensitive.
	Outputs map[string]bool
}

// ReadModule reads the variables and outputs of the module in dir.
func ReadModule(dir string) (Module, error) {
	m := Module{Variables: map[string]bool{}, Outputs: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return m, err
	}
	schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}, {Type: "output", LabelNames: []string{"name"}}}}
	inner := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "default"}, {Name: "sensitive"}}}
	p := hclparse.NewParser()
	for _, e := range entries {
		name := filepath.Join(dir, e.Name())
		var f *hcl.File
		var diags hcl.Diagnostics
		switch {
		case e.IsDir():
			continue
		case strings.HasSuffix(e.Name(), ".tf") || strings.HasSuffix(e.Name(), ".tofu"):
			f, diags = p.ParseHCLFile(name)
		case strings.HasSuffix(e.Name(), ".tf.json") || strings.HasSuffix(e.Name(), ".tofu.json"):
			f, diags = p.ParseJSONFile(name)
		default:
			continue
		}
		if diags.HasErrors() {
			return m, diags
		}
		content, _, diags := f.Body.PartialContent(schema)
		if diags.HasErrors() {
			return m, diags
		}
		for _, b := range content.Blocks {
			attrs, _, _ := b.Body.PartialContent(inner)
			if b.Type == "variable" {
				m.Variables[b.Labels[0]] = attrs.Attributes["default"] != nil
				continue
			}
			sensitive := false
			if a := attrs.Attributes["sensitive"]; a != nil {
				v, diags := a.Expr.Value(nil)
				sensitive = !diags.HasErrors() && v.Type() == cty.Bool && v.True()
			}
			m.Outputs[b.Labels[0]] = sensitive
		}
	}
	return m, nil
}

type edit struct {
	start, end int
	text       string
}

// rewrite returns the source of r with the Stack's names changed to the
// generated module's. eachName is the local that holds each, if any.
func (c *Config) rewrite(r hcl.Range, exprs []hclsyntax.Expression, eachName string) (string, error) {
	var edits []edit
	for _, e := range exprs {
		if e == nil {
			continue
		}
		for _, t := range e.Variables() {
			root := t[0].SourceRange()
			switch t.RootName() {
			case "component":
				edits = append(edits, edit{root.Start.Byte, root.End.Byte, "var.stackwright_components"})
			case "each":
				if eachName == "" {
					return "", invalid(root, "each is only in a block with for_each")
				}
				edits = append(edits, edit{root.Start.Byte, root.End.Byte, "local." + eachName})
			case "provider":
				return "", invalid(root, "a provider is only allowed in providers")
			case "stack":
				return "", &Error{Range: root, Msg: "a reference to an embedded stack", Err: ErrRefused}
			case "terraform":
				return "", &Error{Range: root, Msg: "terraform." + strings.TrimPrefix(c.src(t.SourceRange()), "terraform."), Err: ErrRefused}
			}
		}
	}
	src := c.files[r.Filename]
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := slices.Clone(src[r.Start.Byte:r.End.Byte])
	for _, ed := range edits {
		if ed.start < r.Start.Byte || ed.end > r.End.Byte {
			continue
		}
		s, e := ed.start-r.Start.Byte, ed.end-r.Start.Byte
		out = slices.Concat(out[:s], []byte(ed.text), out[e:])
	}
	return string(out), nil
}

func (c *Config) exprText(e hclsyntax.Expression, eachName string) (string, error) {
	return c.rewrite(e.Range(), []hclsyntax.Expression{e}, eachName)
}

// providerInstance resolves a reference such as provider.aws.this[each.key].
func (c *Config) providerInstance(expr hclsyntax.Expression, ctx *hcl.EvalContext) (*Provider, each, error) {
	var trav hcl.Traversal
	var key hclsyntax.Expression
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		trav = e.Traversal
		if len(trav) == 4 {
			if idx, ok := trav[3].(hcl.TraverseIndex); ok {
				key = &hclsyntax.LiteralValueExpr{Val: idx.Key, SrcRange: idx.SrcRange}
				trav = trav[:3]
			}
		}
	case *hclsyntax.IndexExpr:
		if t, ok := e.Collection.(*hclsyntax.ScopeTraversalExpr); ok {
			trav, key = t.Traversal, e.Key
		}
	}
	if len(trav) != 3 || trav.RootName() != "provider" {
		return nil, each{}, invalid(expr.Range(), "a providers value is a reference such as provider.aws.this")
	}
	t, ok1 := trav[1].(hcl.TraverseAttr)
	n, ok2 := trav[2].(hcl.TraverseAttr)
	if !ok1 || !ok2 {
		return nil, each{}, invalid(expr.Range(), "a providers value is a reference such as provider.aws.this")
	}
	p := c.providers[t.Name+"."+n.Name]
	if p == nil {
		return nil, each{}, invalid(expr.Range(), "provider %q %q is not declared", t.Name, n.Name)
	}
	if (p.forEach == nil) != (key == nil) {
		return nil, each{}, invalid(expr.Range(), "provider %q %q needs a key exactly when it has for_each", t.Name, n.Name)
	}
	if key == nil {
		return p, each{}, nil
	}
	k, err := instanceKey(key, ctx)
	if err != nil {
		return nil, each{}, err
	}
	all, err := forEach(fmt.Sprintf("provider %q %q", t.Name, n.Name), p.forEach, ctx)
	if err != nil {
		return nil, each{}, err
	}
	for _, e := range all {
		if e.key == k {
			return p, e, nil
		}
	}
	return nil, each{}, invalid(expr.Range(), "provider %q %q has no instance %q", t.Name, n.Name, k)
}

// Generated is the root module of one instance and its values.
type Generated struct {
	// Main is the module, for stackwright_stack.tf in the Stack's folder.
	Main []byte
	// Vars holds the values that are not ephemeral, as a .tfvars.json.
	Vars []byte
	// Env holds TF_VAR_ entries for the ephemeral values, which the engine
	// needs at plan and at apply.
	Env []string
}

// Generate writes the root module of an instance (adr/0043). vals are the
// deployment's values; components holds the outputs of the components it
// reads, as JSON, and sensitive says whether any of them is sensitive.
func (c *Config) Generate(inst Instance, vals map[string]cty.Value, components json.RawMessage, sensitive bool, mod Module) (Generated, error) {
	var b strings.Builder
	b.WriteString("# Written by Stackwright from the Stack's files (adr/0043).\n\n")
	ctx := c.stackContext(vals)
	eachName := ""
	if inst.each.value != cty.NilVal {
		eachName = "stackwright_each"
		ctx = ctx.NewChild()
		ctx.Variables = map[string]cty.Value{"each": inst.each.object()}
	}

	var exprs []hclsyntax.Expression
	var providerArgs []providerArg
	source := ""
	var inputs *hclsyntax.ObjectConsExpr
	switch {
	case inst.Component == "":
		for _, o := range c.Outputs {
			exprs = append(exprs, o.value)
		}
	case inst.Removed:
		providerArgs, source = inst.removed.providers, inst.removed.Source
	default:
		comp := c.Components[inst.Component]
		if comp == nil {
			return Generated{}, fmt.Errorf("the Stack has no component %q", inst.Component)
		}
		providerArgs, source, inputs = comp.providers, comp.Source, comp.inputs
		if inputs != nil {
			exprs = append(exprs, inputs)
		}
	}

	type usedProvider struct {
		p     *Provider
		e     each
		alias string
	}
	var used []usedProvider
	var calls []string
	for _, arg := range providerArgs {
		p, e, err := c.providerInstance(arg.expr, ctx)
		if err != nil {
			return Generated{}, err
		}
		i := slices.IndexFunc(used, func(u usedProvider) bool { return u.p == p && u.e.key == e.key })
		if i < 0 {
			used = append(used, usedProvider{p: p, e: e, alias: fmt.Sprintf("%s_%d", p.Name, len(used))})
			i = len(used) - 1
		}
		calls = append(calls, fmt.Sprintf("    %s = %s.%s\n", arg.key, p.Type, used[i].alias))
		exprs = append(exprs, bodyExprs(p.config)...)
	}

	if c.requiredProviders != nil && len(used) > 0 {
		rp := c.requiredProviders
		b.WriteString("terraform {\n  required_providers {")
		b.WriteString(c.src(hcl.Range{Filename: rp.OpenBraceRange.Filename, Start: rp.OpenBraceRange.End, End: rp.CloseBraceRange.Start}))
		b.WriteString("}\n}\n\n")
	}
	fmt.Fprintf(&b, "variable \"stackwright_components\" {\n  type      = any\n  default   = {}\n  sensitive = %t\n}\n\n", sensitive)
	for _, v := range c.Variables {
		b.WriteString(c.src(v.block.Range()))
		b.WriteString("\n\n")
	}

	var locals []string
	seen := map[string]bool{}
	c.refs(exprs, seen, func(hcl.Traversal) {})
	for k := range seen {
		if name, ok := strings.CutPrefix(k, "local."); ok {
			locals = append(locals, name)
		}
	}
	sort.Strings(locals)
	b.WriteString("locals {\n")
	if eachName != "" {
		fmt.Fprintf(&b, "  %s = %s\n", eachName, hclwrite.TokensForValue(inst.each.object()).Bytes())
	}
	for i, u := range used {
		if u.p.forEach != nil {
			fmt.Fprintf(&b, "  stackwright_provider_%d = %s\n", i, hclwrite.TokensForValue(u.e.object()).Bytes())
		}
	}
	for _, name := range locals {
		text, err := c.exprText(c.locals[name], "")
		if err != nil {
			return Generated{}, err
		}
		fmt.Fprintf(&b, "  %s = %s\n", name, text)
	}
	b.WriteString("}\n\n")

	for i, u := range used {
		fmt.Fprintf(&b, "provider %q {\n  alias = %q\n", u.p.Type, u.alias)
		if u.p.config != nil {
			cfg := u.p.config
			r := hcl.Range{Filename: cfg.OpenBraceRange.Filename, Start: cfg.OpenBraceRange.End, End: cfg.CloseBraceRange.Start}
			name := ""
			if u.p.forEach != nil {
				name = fmt.Sprintf("stackwright_provider_%d", i)
			}
			text, err := c.rewrite(r, bodyExprs(cfg), name)
			if err != nil {
				return Generated{}, err
			}
			b.WriteString(text)
		}
		b.WriteString("\n}\n\n")
	}

	if inst.Component != "" {
		fmt.Fprintf(&b, "module %q {\n  source = %q\n", inst.Component, "./"+source)
		if len(calls) > 0 {
			b.WriteString("  providers = {\n" + strings.Join(calls, "") + "  }\n")
		}
		given := map[string]bool{}
		if inputs != nil {
			for _, item := range inputs.Items {
				key := hcl.ExprAsKeyword(item.KeyExpr)
				if key == "" {
					v, diags := item.KeyExpr.Value(nil)
					if diags.HasErrors() || v.Type() != cty.String {
						return Generated{}, invalid(item.KeyExpr.Range(), "an input name must be a variable name")
					}
					key = v.AsString()
				}
				if _, ok := mod.Variables[key]; !ok {
					continue
				}
				text, err := c.exprText(item.ValueExpr, eachName)
				if err != nil {
					return Generated{}, err
				}
				fmt.Fprintf(&b, "  %s = %s\n", key, text)
				given[key] = true
			}
		}
		if inst.Removed {
			names := make([]string, 0, len(mod.Variables))
			for name, hasDefault := range mod.Variables {
				if !hasDefault && !given[name] {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&b, "  %s = null\n", name)
			}
		}
		b.WriteString("}\n\n")
		if !inst.Removed {
			names := make([]string, 0, len(mod.Outputs))
			for name := range mod.Outputs {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&b, "output %q {\n  value     = module.%s.%s\n  sensitive = %t\n}\n\n", name, inst.Component, name, mod.Outputs[name] || sensitive)
			}
		}
	} else {
		for _, o := range c.Outputs {
			text, err := c.exprText(o.value, "")
			if err != nil {
				return Generated{}, err
			}
			fmt.Fprintf(&b, "output %q {\n  value     = %s\n  sensitive = %t\n}\n\n", o.Name, text, o.Sensitive || sensitive)
		}
	}

	g := Generated{Main: hclwrite.Format([]byte(b.String()))}
	plain := map[string]json.RawMessage{"stackwright_components": components}
	if len(components) == 0 {
		plain["stackwright_components"] = json.RawMessage("{}")
	}
	for _, v := range c.Variables {
		val := vals[v.Name]
		if !val.IsWhollyKnown() {
			return Generated{}, fmt.Errorf("variable %q has no value yet", v.Name)
		}
		js, err := ctyjson.Marshal(val, val.Type())
		if err != nil {
			return Generated{}, fmt.Errorf("variable %q: %w", v.Name, err)
		}
		if !v.Ephemeral {
			plain[v.Name] = js
			continue
		}
		text := string(js)
		if val.Type() == cty.String && !val.IsNull() {
			text = val.AsString()
		}
		g.Env = append(g.Env, "TF_VAR_"+v.Name+"="+text)
	}
	var err error
	if g.Vars, err = json.MarshalIndent(plain, "", "  "); err != nil {
		return Generated{}, err
	}
	if _, diags := hclsyntax.ParseConfig(g.Main, "stackwright_stack.tf", hcl.InitialPos); diags.HasErrors() {
		return Generated{}, errors.Join(errors.New("the generated module is not valid HCL"), diags)
	}
	return g, nil
}

// SourceOf returns the folder of an instance's module, relative to the
// Stack's folder.
func (c *Config) SourceOf(i Instance) string {
	if i.Removed {
		return i.removed.Source
	}
	if comp := c.Components[i.Component]; comp != nil {
		return comp.Source
	}
	return ""
}

// Find returns the instance of a deployment with a component and key, as
// Instances lists it. An empty component is the deployment's outputs.
func (c *Config) Find(deployment, component, key string, keyed bool, vals map[string]cty.Value) (Instance, error) {
	var prior []Prior
	if component != "" {
		prior = []Prior{{Component: component, Key: key, Keyed: keyed}}
	}
	all, err := c.Instances(deployment, vals, prior)
	if err != nil {
		return Instance{}, err
	}
	for _, i := range all {
		if i.Component == component && i.Key == key && i.Keyed == keyed {
			return i, nil
		}
	}
	return Instance{}, fmt.Errorf("deployment %q has no instance of component %q with key %q", deployment, component, key)
}

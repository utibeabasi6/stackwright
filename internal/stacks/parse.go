// Package stacks reads Terraform Stacks files and writes, for each
// component instance, a root module that OpenTofu or Terraform runs
// (adr/0019, adr/0043).
package stacks

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Format is the version of the Stacks format that this package follows.
const Format = "Terraform v1.17.0 with the tfstacks plugin v1.4.0"

const (
	componentSuffix  = ".tfcomponent.hcl"
	deploymentSuffix = ".tfdeploy.hcl"
)

// ErrRefused marks a block or argument that Stackwright does not run yet.
var ErrRefused = errors.New("is not supported by Stackwright yet")

// IsConfig reports whether OpenTofu reads the file as configuration.
// https://opentofu.org/docs/language/files/
func IsConfig(name string) bool {
	for _, ext := range []string{".tf", ".tofu", ".tf.json", ".tofu.json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// IsStackFile reports whether name is a Stack's component or deployment
// file, in HCL or JSON.
func IsStackFile(name string) bool {
	for _, s := range []string{componentSuffix, deploymentSuffix, ".tfcomponent.json", ".tfdeploy.json"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// FilesIn returns the Stack files at the top of folder, by name, from a
// repository's files by path. folder "" is the repository's root.
func FilesIn(files map[string][]byte, folder string) map[string][]byte {
	out := map[string][]byte{}
	for name, b := range files {
		dir := path.Dir(name)
		if dir == "." {
			dir = ""
		}
		if dir == folder && IsStackFile(name) {
			out[path.Base(name)] = b
		}
	}
	return out
}

// Error is a problem in a Stack's files and where it is.
type Error struct {
	Range hcl.Range
	Msg   string
	Err   error
}

func (e *Error) Error() string {
	msg := e.Msg
	if e.Err != nil {
		msg += " " + e.Err.Error()
	}
	return fmt.Sprintf("%s:%d: %s", e.Range.Filename, e.Range.Start.Line, msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Config is a Stack: its component files and its deployment files.
type Config struct {
	files             map[string][]byte
	requiredProviders *hclsyntax.Block
	Variables         []*Variable
	locals            map[string]hclsyntax.Expression
	providers         map[string]*Provider
	Components        map[string]*Component
	Removed           []*Removed
	Outputs           []*Output
	Deployments       []*Deployment
	deployLocals      map[string]hclsyntax.Expression
	tokens            map[string]hclsyntax.Expression
	stores            map[string]string
	// Refused lists what the Stack uses that Stackwright refuses. A plan of
	// the Stack fails with them; a sync still reads the rest.
	Refused []error
}

type Variable struct {
	Name      string
	Ephemeral bool
	block     *hclsyntax.Block
}

// Provider is a provider configuration, with for_each or without.
type Provider struct {
	Type, Name string
	rng        hcl.Range
	forEach    hclsyntax.Expression
	config     *hclsyntax.Block
}

type providerArg struct {
	key  string
	expr hclsyntax.Expression
}

type Component struct {
	Name      string
	Source    string
	Range     hcl.Range
	inputs    *hclsyntax.ObjectConsExpr
	providers []providerArg
	forEach   hclsyntax.Expression
	dependsOn []string
}

type Removed struct {
	Component string
	Source    string
	Destroy   bool
	Range     hcl.Range
	key       hclsyntax.Expression
	providers []providerArg
	forEach   hclsyntax.Expression
}

type Output struct {
	Name      string
	Sensitive bool
	value     hclsyntax.Expression
}

type Deployment struct {
	Name    string
	Group   string
	Destroy bool
	inputs  *hclsyntax.ObjectConsExpr
}

// Parse reads a Stack from the files at the top of its folder, by name.
// It returns an error for files that are not valid Stacks files; what
// Stackwright refuses goes in Config.Refused.
func Parse(files map[string][]byte) (*Config, error) {
	c := &Config{files: files, locals: map[string]hclsyntax.Expression{}, providers: map[string]*Provider{},
		Components: map[string]*Component{}, deployLocals: map[string]hclsyntax.Expression{},
		tokens: map[string]hclsyntax.Expression{}, stores: map[string]string{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	var components, deployments int
	for _, name := range names {
		switch {
		case strings.HasSuffix(name, ".tfcomponent.json"), strings.HasSuffix(name, ".tfdeploy.json"):
			c.refuse(hcl.Range{Filename: name, Start: hcl.Pos{Line: 1}}, "a Stacks file in JSON")
			continue
		case strings.HasSuffix(name, componentSuffix):
			components++
		case strings.HasSuffix(name, deploymentSuffix):
			deployments++
		default:
			continue
		}
		f, diags := hclsyntax.ParseConfig(files[name], name, hcl.InitialPos)
		if diags.HasErrors() {
			errs = append(errs, diags)
			continue
		}
		body := f.Body.(*hclsyntax.Body)
		var err error
		if strings.HasSuffix(name, componentSuffix) {
			err = c.component(body)
		} else {
			err = c.deployment(body)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if components == 0 || deployments == 0 {
		errs = append(errs, errors.New("a Stack needs at least one .tfcomponent.hcl file and one .tfdeploy.hcl file"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.SortFunc(c.Variables, func(a, b *Variable) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(c.Outputs, func(a, b *Output) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(c.Deployments, func(a, b *Deployment) int { return strings.Compare(a.Name, b.Name) })
	return c, c.check()
}

func (c *Config) refuse(r hcl.Range, what string) {
	c.Refused = append(c.Refused, &Error{Range: r, Msg: what, Err: ErrRefused})
}

func invalid(r hcl.Range, format string, a ...any) error {
	return &Error{Range: r, Msg: fmt.Sprintf(format, a...)}
}

func (c *Config) src(r hcl.Range) string {
	return string(c.files[r.Filename][r.Start.Byte:r.End.Byte])
}

// literal is an argument that takes no references: its value with no
// evaluation context.
func literal(attr *hclsyntax.Attribute) (cty.Value, error) {
	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, invalid(attr.SrcRange, "%s must be a literal value", attr.Name)
	}
	return v, nil
}

func literalString(attr *hclsyntax.Attribute) (string, error) {
	v, err := literal(attr)
	if err != nil {
		return "", err
	}
	if v.IsNull() || v.Type() != cty.String {
		return "", invalid(attr.SrcRange, "%s must be a string", attr.Name)
	}
	return v.AsString(), nil
}

func literalBool(attr *hclsyntax.Attribute) (bool, error) {
	v, err := literal(attr)
	if err != nil {
		return false, err
	}
	if v.IsNull() || v.Type() != cty.Bool {
		return false, invalid(attr.SrcRange, "%s must be true or false", attr.Name)
	}
	return v.True(), nil
}

// only checks a body's arguments and nested blocks against those allowed.
func only(b *hclsyntax.Body, attrs []string, blocks []string) error {
	for name, a := range b.Attributes {
		if !slices.Contains(attrs, name) {
			return invalid(a.NameRange, "an argument named %q is not expected here", name)
		}
	}
	for _, nb := range b.Blocks {
		if !slices.Contains(blocks, nb.Type) {
			return invalid(nb.TypeRange, "a block named %q is not expected here", nb.Type)
		}
	}
	return nil
}

func labels(b *hclsyntax.Block, n int) error {
	if len(b.Labels) != n {
		return invalid(b.TypeRange, "a %s block needs %d label(s)", b.Type, n)
	}
	for i, l := range b.Labels {
		if !hclsyntax.ValidIdentifier(l) {
			return invalid(b.LabelRanges[i], "%q is not a valid name", l)
		}
	}
	return nil
}

func (c *Config) component(body *hclsyntax.Body) error {
	for name, a := range body.Attributes {
		if name != "language" {
			return invalid(a.NameRange, "an argument named %q is not expected here", name)
		}
	}
	for _, b := range body.Blocks {
		switch b.Type {
		case "required_providers":
			if c.requiredProviders != nil {
				return invalid(b.TypeRange, "a Stack may have only one required_providers block")
			}
			if len(b.Body.Blocks) > 0 {
				return invalid(b.Body.Blocks[0].TypeRange, "required_providers takes only arguments")
			}
			c.requiredProviders = b
		case "variable":
			if err := c.variable(b); err != nil {
				return err
			}
		case "locals":
			if err := addLocals(c.locals, b); err != nil {
				return err
			}
		case "provider":
			if err := c.provider(b); err != nil {
				return err
			}
		case "component":
			if err := c.componentBlock(b); err != nil {
				return err
			}
		case "removed":
			if err := c.removed(b); err != nil {
				return err
			}
		case "output":
			if err := c.output(b); err != nil {
				return err
			}
		case "stack":
			c.refuse(b.TypeRange, "an embedded stack block")
		default:
			return invalid(b.TypeRange, "a block named %q is not expected in a component file", b.Type)
		}
	}
	return nil
}

func (c *Config) variable(b *hclsyntax.Block) error {
	if err := labels(b, 1); err != nil {
		return err
	}
	if err := only(b.Body, []string{"type", "default", "description", "sensitive", "ephemeral"}, []string{"validation"}); err != nil {
		return err
	}
	name := b.Labels[0]
	if strings.HasPrefix(name, "stackwright_") {
		return invalid(b.LabelRanges[0], "names that start with stackwright_ are for Stackwright")
	}
	if slices.ContainsFunc(c.Variables, func(v *Variable) bool { return v.Name == name }) {
		return invalid(b.LabelRanges[0], "variable %q is declared twice", name)
	}
	if b.Body.Attributes["type"] == nil {
		return invalid(b.TypeRange, "variable %q needs a type", name)
	}
	v := &Variable{Name: name, block: b}
	if a := b.Body.Attributes["ephemeral"]; a != nil {
		var err error
		if v.Ephemeral, err = literalBool(a); err != nil {
			return err
		}
	}
	c.Variables = append(c.Variables, v)
	return nil
}

func addLocals(into map[string]hclsyntax.Expression, b *hclsyntax.Block) error {
	if err := labels(b, 0); err != nil {
		return err
	}
	if len(b.Body.Blocks) > 0 {
		return invalid(b.Body.Blocks[0].TypeRange, "locals takes only arguments")
	}
	for name, a := range b.Body.Attributes {
		if strings.HasPrefix(name, "stackwright_") {
			return invalid(a.NameRange, "names that start with stackwright_ are for Stackwright")
		}
		if into[name] != nil {
			return invalid(a.NameRange, "local %q is declared twice", name)
		}
		into[name] = a.Expr
	}
	return nil
}

func (c *Config) provider(b *hclsyntax.Block) error {
	if err := labels(b, 2); err != nil {
		return err
	}
	if err := only(b.Body, []string{"for_each"}, []string{"config"}); err != nil {
		return err
	}
	p := &Provider{Type: b.Labels[0], Name: b.Labels[1], rng: b.TypeRange}
	key := p.Type + "." + p.Name
	if c.providers[key] != nil {
		return invalid(b.TypeRange, "provider %q %q is declared twice", p.Type, p.Name)
	}
	if a := b.Body.Attributes["for_each"]; a != nil {
		p.forEach = a.Expr
	}
	for _, nb := range b.Body.Blocks {
		if p.config != nil {
			return invalid(nb.TypeRange, "a provider takes one config block")
		}
		if err := labels(nb, 0); err != nil {
			return err
		}
		p.config = nb
	}
	c.providers[key] = p
	return nil
}

func (c *Config) providerArgs(a *hclsyntax.Attribute) ([]providerArg, error) {
	obj, ok := a.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil, invalid(a.SrcRange, "providers must be an object such as { aws = provider.aws.this }")
	}
	var out []providerArg
	for _, item := range obj.Items {
		trav, diags := hcl.AbsTraversalForExpr(item.KeyExpr)
		if diags.HasErrors() || len(trav) == 0 || len(trav) > 2 {
			return nil, invalid(item.KeyExpr.Range(), "a providers key is a provider name, or a name and an alias")
		}
		bad := false
		key := trav.RootName()
		if len(trav) == 2 {
			attr, ok := trav[1].(hcl.TraverseAttr)
			bad = bad || !ok
			key += "." + attr.Name
		}
		if bad || key == "" {
			return nil, invalid(item.KeyExpr.Range(), "a providers key is a provider name, or a name and an alias")
		}
		if slices.ContainsFunc(out, func(p providerArg) bool { return p.key == key }) {
			return nil, invalid(item.KeyExpr.Range(), "provider %q is given twice", key)
		}
		out = append(out, providerArg{key: key, expr: item.ValueExpr})
	}
	return out, nil
}

// source reads a component's or a removed block's source and version.
// Stackwright runs local sources only.
func (c *Config) source(b *hclsyntax.Block) (string, error) {
	a := b.Body.Attributes["source"]
	if a == nil {
		return "", invalid(b.TypeRange, "a %s block needs a source", b.Type)
	}
	s, err := literalString(a)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
		c.refuse(a.SrcRange, fmt.Sprintf("the source %q, which is not a local path,", s))
		return s, nil
	}
	if v := b.Body.Attributes["version"]; v != nil {
		return "", invalid(v.SrcRange, "a local source takes no version")
	}
	return path.Clean(path.Join(path.Dir(b.TypeRange.Filename), s)), nil
}

func (c *Config) componentBlock(b *hclsyntax.Block) error {
	if err := labels(b, 1); err != nil {
		return err
	}
	if err := only(b.Body, []string{"source", "version", "inputs", "providers", "for_each", "depends_on"}, nil); err != nil {
		return err
	}
	name := b.Labels[0]
	if c.Components[name] != nil {
		return invalid(b.LabelRanges[0], "component %q is declared twice", name)
	}
	comp := &Component{Name: name, Range: b.Range()}
	var err error
	if comp.Source, err = c.source(b); err != nil {
		return err
	}
	if a := b.Body.Attributes["inputs"]; a != nil {
		obj, ok := a.Expr.(*hclsyntax.ObjectConsExpr)
		if !ok {
			c.refuse(a.SrcRange, "inputs that are not an object such as { name = var.name }")
		}
		comp.inputs = obj
	}
	if a := b.Body.Attributes["providers"]; a != nil {
		if comp.providers, err = c.providerArgs(a); err != nil {
			return err
		}
	}
	if a := b.Body.Attributes["for_each"]; a != nil {
		comp.forEach = a.Expr
	}
	if a := b.Body.Attributes["depends_on"]; a != nil {
		tuple, ok := a.Expr.(*hclsyntax.TupleConsExpr)
		if !ok {
			return invalid(a.SrcRange, "depends_on must be a list of components")
		}
		for _, e := range tuple.Exprs {
			trav, diags := hcl.AbsTraversalForExpr(e)
			if diags.HasErrors() || len(trav) != 2 || trav.RootName() != "component" {
				if !diags.HasErrors() && trav.RootName() == "stack" {
					c.refuse(e.Range(), "depends_on an embedded stack")
					continue
				}
				return invalid(e.Range(), "depends_on takes references such as component.network")
			}
			attr, ok := trav[1].(hcl.TraverseAttr)
			if !ok {
				return invalid(e.Range(), "depends_on takes references such as component.network")
			}
			comp.dependsOn = append(comp.dependsOn, attr.Name)
		}
	}
	c.Components[name] = comp
	return nil
}

func (c *Config) removed(b *hclsyntax.Block) error {
	if err := labels(b, 0); err != nil {
		return err
	}
	if err := only(b.Body, []string{"from", "source", "version", "providers", "for_each", "inputs"}, []string{"lifecycle"}); err != nil {
		return err
	}
	r := &Removed{Destroy: true, Range: b.Range()}
	from := b.Body.Attributes["from"]
	if from == nil {
		return invalid(b.TypeRange, "a removed block needs from")
	}
	switch e := from.Expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		if len(e.Traversal) == 3 {
			if idx, ok := e.Traversal[2].(hcl.TraverseIndex); ok {
				r.key = &hclsyntax.LiteralValueExpr{Val: idx.Key, SrcRange: idx.SrcRange}
				e = &hclsyntax.ScopeTraversalExpr{Traversal: e.Traversal[:2], SrcRange: e.SrcRange}
			}
		}
		if len(e.Traversal) != 2 || e.Traversal.RootName() != "component" {
			if e.Traversal.RootName() == "stack" {
				c.refuse(from.SrcRange, "removing an embedded stack")
				return nil
			}
			return invalid(from.SrcRange, "from takes a reference such as component.network or component.network[each.key]")
		}
		attr, ok := e.Traversal[1].(hcl.TraverseAttr)
		if !ok {
			return invalid(from.SrcRange, "from takes a reference such as component.network or component.network[each.key]")
		}
		r.Component = attr.Name
	case *hclsyntax.IndexExpr:
		trav, diags := hcl.AbsTraversalForExpr(e.Collection)
		if diags.HasErrors() || len(trav) != 2 || trav.RootName() != "component" {
			return invalid(from.SrcRange, "from takes a reference such as component.network[each.key]")
		}
		attr, ok := trav[1].(hcl.TraverseAttr)
		if !ok {
			return invalid(from.SrcRange, "from takes a reference such as component.network[each.key]")
		}
		r.Component, r.key = attr.Name, e.Key
	default:
		return invalid(from.SrcRange, "from takes a reference such as component.network")
	}
	var err error
	if r.Source, err = c.source(b); err != nil {
		return err
	}
	if a := b.Body.Attributes["inputs"]; a != nil {
		return invalid(a.SrcRange, "inputs is only for removing an embedded stack")
	}
	if a := b.Body.Attributes["providers"]; a != nil {
		if r.providers, err = c.providerArgs(a); err != nil {
			return err
		}
	}
	if a := b.Body.Attributes["for_each"]; a != nil {
		r.forEach = a.Expr
		uses := false
		if r.key != nil {
			for _, t := range r.key.Variables() {
				uses = uses || t.RootName() == "each"
			}
		}
		if !uses {
			return invalid(from.SrcRange, "a removed block with for_each must use each in from")
		}
	}
	for _, nb := range b.Body.Blocks {
		if err := only(nb.Body, []string{"destroy"}, nil); err != nil {
			return err
		}
		if a := nb.Body.Attributes["destroy"]; a != nil {
			if r.Destroy, err = literalBool(a); err != nil {
				return err
			}
		}
	}
	c.Removed = append(c.Removed, r)
	return nil
}

func (c *Config) output(b *hclsyntax.Block) error {
	if err := labels(b, 1); err != nil {
		return err
	}
	if err := only(b.Body, []string{"type", "value", "description", "sensitive", "ephemeral"}, []string{"precondition"}); err != nil {
		return err
	}
	o := &Output{Name: b.Labels[0]}
	if slices.ContainsFunc(c.Outputs, func(x *Output) bool { return x.Name == o.Name }) {
		return invalid(b.LabelRanges[0], "output %q is declared twice", o.Name)
	}
	if b.Body.Attributes["type"] == nil || b.Body.Attributes["value"] == nil {
		return invalid(b.TypeRange, "output %q needs a type and a value", o.Name)
	}
	if len(b.Body.Blocks) > 0 {
		return invalid(b.Body.Blocks[0].TypeRange, "stack outputs do not support preconditions")
	}
	o.value = b.Body.Attributes["value"].Expr
	var err error
	if a := b.Body.Attributes["sensitive"]; a != nil {
		if o.Sensitive, err = literalBool(a); err != nil {
			return err
		}
	}
	if a := b.Body.Attributes["ephemeral"]; a != nil {
		ephemeral, err := literalBool(a)
		if err != nil {
			return err
		}
		if ephemeral {
			c.refuse(a.SrcRange, "an ephemeral stack output")
		}
	}
	c.Outputs = append(c.Outputs, o)
	return nil
}

func (c *Config) deployment(body *hclsyntax.Body) error {
	for name, a := range body.Attributes {
		return invalid(a.NameRange, "an argument named %q is not expected in a deployment file", name)
	}
	for _, b := range body.Blocks {
		switch b.Type {
		case "deployment":
			if err := c.deploymentBlock(b); err != nil {
				return err
			}
		case "deployment_group":
			if err := labels(b, 1); err != nil {
				return err
			}
			for name, a := range b.Body.Attributes {
				switch name {
				case "auto_approve_checks", "eager_plan", "failure_tolerance":
					c.refuse(a.NameRange, name+" in a deployment_group")
				default:
					return invalid(a.NameRange, "an argument named %q is not expected here", name)
				}
			}
		case "identity_token":
			if err := labels(b, 1); err != nil {
				return err
			}
			if err := only(b.Body, []string{"audience"}, nil); err != nil {
				return err
			}
			a := b.Body.Attributes["audience"]
			if a == nil {
				return invalid(b.TypeRange, "identity_token %q needs an audience", b.Labels[0])
			}
			c.tokens[b.Labels[0]] = a.Expr
		case "store":
			if err := labels(b, 2); err != nil {
				return err
			}
			if b.Labels[0] != "varset" {
				c.refuse(b.TypeRange, fmt.Sprintf("store %q", b.Labels[0]))
				continue
			}
			if err := only(b.Body, []string{"id", "name", "category"}, nil); err != nil {
				return err
			}
			a := b.Body.Attributes["category"]
			if a == nil {
				return invalid(b.TypeRange, "store %q needs a category", b.Labels[1])
			}
			cat, err := literalString(a)
			if err != nil {
				return err
			}
			if cat != "terraform" && cat != "env" {
				return invalid(a.SrcRange, `category must be "terraform" or "env"`)
			}
			c.stores[b.Labels[1]] = cat
		case "locals":
			if err := addLocals(c.deployLocals, b); err != nil {
				return err
			}
		case "deployment_auto_approve", "orchestrate", "publish_output", "upstream_input":
			c.refuse(b.TypeRange, "a "+b.Type+" block")
		default:
			return invalid(b.TypeRange, "a block named %q is not expected in a deployment file", b.Type)
		}
	}
	return nil
}

func (c *Config) deploymentBlock(b *hclsyntax.Block) error {
	if err := labels(b, 1); err != nil {
		return err
	}
	if err := only(b.Body, []string{"inputs", "deployment_group", "destroy", "import", "migrate"}, nil); err != nil {
		return err
	}
	d := &Deployment{Name: b.Labels[0]}
	if slices.ContainsFunc(c.Deployments, func(x *Deployment) bool { return x.Name == d.Name }) {
		return invalid(b.LabelRanges[0], "deployment %q is declared twice", d.Name)
	}
	if a := b.Body.Attributes["inputs"]; a != nil {
		obj, ok := a.Expr.(*hclsyntax.ObjectConsExpr)
		if !ok {
			return invalid(a.SrcRange, "inputs must be an object such as { region = \"us-east-1\" }")
		}
		d.inputs = obj
	}
	if a := b.Body.Attributes["deployment_group"]; a != nil {
		trav, diags := hcl.AbsTraversalForExpr(a.Expr)
		if diags.HasErrors() || len(trav) != 2 || trav.RootName() != "deployment_group" {
			return invalid(a.SrcRange, "deployment_group takes a reference such as deployment_group.canary")
		}
		attr, ok := trav[1].(hcl.TraverseAttr)
		if !ok {
			return invalid(a.SrcRange, "deployment_group takes a reference such as deployment_group.canary")
		}
		d.Group = attr.Name
	}
	var err error
	if a := b.Body.Attributes["destroy"]; a != nil {
		if d.Destroy, err = literalBool(a); err != nil {
			return err
		}
	}
	for _, name := range []string{"import", "migrate"} {
		if a := b.Body.Attributes[name]; a != nil {
			on, err := literalBool(a)
			if err != nil {
				return err
			}
			if on {
				c.refuse(a.SrcRange, name+" in a deployment")
			}
		}
	}
	c.Deployments = append(c.Deployments, d)
	return nil
}

// check finds references to things the Stack does not declare.
func (c *Config) check() error {
	if len(c.Deployments) == 0 {
		return errors.New("the Stack has no deployment blocks")
	}
	names := map[string]bool{}
	if c.requiredProviders != nil {
		for name := range c.requiredProviders.Body.Attributes {
			names[name] = true
		}
	}
	for _, p := range c.providers {
		if !names[p.Type] {
			return invalid(p.rng, "provider %q is not in required_providers", p.Type)
		}
	}
	for _, comp := range c.Components {
		for _, dep := range comp.dependsOn {
			if c.Components[dep] == nil {
				return invalid(comp.Range, "component %q depends on component %q, which does not exist", comp.Name, dep)
			}
		}
	}
	for _, r := range c.Removed {
		if c.Components[r.Component] != nil && r.key == nil {
			return invalid(r.Range, "component %q exists, so a removed block must name its instances", r.Component)
		}
	}
	return nil
}

package toolbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/stackwright/stackwright/internal/toolbox/guard"
)

const (
	maxRules       = 16 << 10
	maxFacts       = 60
	maxDescription = 160
)

func (b *box) init(ctx context.Context) (string, error) {
	return b.engineRun(ctx, "init", "-backend=false", "-input=false", "-no-color")
}

type schemaFile struct {
	ProviderSchemas map[string]struct {
		ResourceSchemas   map[string]struct{ Block schemaBlock } `json:"resource_schemas"`
		DataSourceSchemas map[string]struct{ Block schemaBlock } `json:"data_source_schemas"`
	} `json:"provider_schemas"`
}

type schemaBlock struct {
	Attributes  map[string]schemaAttr   `json:"attributes"`
	BlockTypes  map[string]schemaNested `json:"block_types"`
	Description string                  `json:"description"`
	Deprecated  bool                    `json:"deprecated"`
}

type schemaNested struct {
	NestingMode string      `json:"nesting_mode"`
	Block       schemaBlock `json:"block"`
	MinItems    int         `json:"min_items"`
	MaxItems    int         `json:"max_items"`
}

type schemaAttr struct {
	Type       json.RawMessage `json:"type"`
	NestedType *struct {
		Attributes  map[string]schemaAttr `json:"attributes"`
		NestingMode string                `json:"nesting_mode"`
	} `json:"nested_type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Optional    bool   `json:"optional"`
	Computed    bool   `json:"computed"`
	Sensitive   bool   `json:"sensitive"`
	Deprecated  bool   `json:"deprecated"`
}

// providerSchema describes one resource or data source type from the
// schemas of the providers that init installed. The schemas are kept in
// the work folder until the lock file changes.
func (b *box) providerSchema(ctx context.Context, typ, kind string) (string, error) {
	if typ == "" {
		return "", errors.New("name a resource type, such as aws_s3_bucket")
	}
	data := kind == "data" || kind == "data_source"
	schemas, key := filepath.Join(b.work, "schema.json"), filepath.Join(b.work, "schema.lock")
	lock, _ := os.ReadFile(filepath.Join(b.dir, ".terraform.lock.hcl"))
	var raw []byte
	if kept, err := os.ReadFile(key); err == nil && bytes.Equal(kept, lock) { //nolint:gosec // a file this package writes
		raw, _ = os.ReadFile(schemas) //nolint:gosec // a file this package writes
	}
	for fresh := raw == nil; ; fresh = true {
		if fresh {
			if out, err := b.init(ctx); err != nil {
				return out, err
			}
			ctx, cancel := context.WithTimeout(ctx, commandTimeout)
			var stderr bytes.Buffer
			cmd := b.engineCmd(ctx, "providers", "schema", "-json")
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			cancel()
			if err != nil {
				return stderr.String(), fmt.Errorf("%s providers schema failed: %w\n%s", filepath.Base(b.engine), err, stderr.String())
			}
			raw = out
			lock, _ = os.ReadFile(filepath.Join(b.dir, ".terraform.lock.hcl"))
			_ = os.WriteFile(schemas, raw, 0o600)
			_ = os.WriteFile(key, lock, 0o600) //nolint:gosec // a file this package writes
		}
		var f schemaFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", fmt.Errorf("the providers' schemas are not JSON: %w", err)
		}
		if out, ok := describeType(&f, typ, data); ok || fresh {
			return out, nil
		}
	}
}

// describeType returns the schema of typ as text, or names of similar
// types when no provider has it.
func describeType(f *schemaFile, typ string, data bool) (string, bool) {
	what := "resource"
	if data {
		what = "data source"
	}
	var names []string
	for source, p := range f.ProviderSchemas {
		types := p.ResourceSchemas
		if data {
			types = p.DataSourceSchemas
		}
		if s, ok := types[typ]; ok {
			var w strings.Builder
			fmt.Fprintf(&w, "%s %s, from %s\n", what, typ, source)
			if s.Block.Deprecated {
				w.WriteString("This type is deprecated.\n")
			}
			describeBlock(&w, s.Block, "")
			return w.String(), true
		}
		for name := range types {
			names = append(names, name)
		}
	}
	if len(f.ProviderSchemas) == 0 {
		return "No provider is installed. Add a required_providers entry for the provider first.", false
	}
	return fmt.Sprintf("No installed provider has a %s %s.%s", what, typ, similar(typ, names)), false
}

// similar names up to 10 types that share the most words with typ.
func similar(typ string, names []string) string {
	words := strings.Split(typ, "_")
	score := func(name string) int {
		n := 0
		for _, w := range words {
			for _, nw := range strings.Split(name, "_") {
				if w == nw || (min(len(w), len(nw)) >= 3 && (strings.HasPrefix(w, nw) || strings.HasPrefix(nw, w))) {
					n++
					break
				}
			}
		}
		return n
	}
	names = slices.DeleteFunc(names, func(n string) bool { return score(n) < 2 })
	slices.SortFunc(names, func(a, b string) int {
		if d := score(b) - score(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	if len(names) == 0 {
		return ""
	}
	return " Similar types: " + strings.Join(names[:min(len(names), 10)], ", ") + "."
}

func describeBlock(w *strings.Builder, b schemaBlock, indent string) {
	names := make([]string, 0, len(b.Attributes))
	for n := range b.Attributes {
		names = append(names, n)
	}
	rank := func(a schemaAttr) int {
		switch {
		case a.Required:
			return 0
		case a.Optional:
			return 1
		}
		return 2
	}
	slices.SortFunc(names, func(x, y string) int {
		if d := rank(b.Attributes[x]) - rank(b.Attributes[y]); d != 0 {
			return d
		}
		return strings.Compare(x, y)
	})
	for _, n := range names {
		a := b.Attributes[n]
		flags := []string{"read-only"}
		switch {
		case a.Required:
			flags = []string{"required"}
		case a.Optional:
			flags = []string{"optional"}
		}
		if a.Sensitive {
			flags = append(flags, "sensitive")
		}
		if a.Deprecated {
			flags = append(flags, "deprecated")
		}
		typ := ctyType(a.Type)
		if a.NestedType != nil {
			typ = a.NestedType.NestingMode + " of objects"
		}
		fmt.Fprintf(w, "%s%s: %s, %s%s\n", indent, n, typ, strings.Join(flags, ", "), shortDescription(a.Description))
		if a.NestedType != nil {
			describeBlock(w, schemaBlock{Attributes: a.NestedType.Attributes}, indent+"  ")
		}
	}
	blocks := make([]string, 0, len(b.BlockTypes))
	for n := range b.BlockTypes {
		blocks = append(blocks, n)
	}
	slices.Sort(blocks)
	for _, n := range blocks {
		nb := b.BlockTypes[n]
		var limits []string
		if nb.MinItems > 0 {
			limits = append(limits, fmt.Sprintf("at least %d", nb.MinItems))
		}
		if nb.MaxItems > 0 {
			limits = append(limits, fmt.Sprintf("at most %d", nb.MaxItems))
		}
		if nb.Block.Deprecated {
			limits = append(limits, "deprecated")
		}
		extra := ""
		if len(limits) > 0 {
			extra = ", " + strings.Join(limits, ", ")
		}
		fmt.Fprintf(w, "%sblock %s (%s%s)%s\n", indent, n, nb.NestingMode, extra, shortDescription(nb.Block.Description))
		describeBlock(w, nb.Block, indent+"  ")
	}
}

func shortDescription(d string) string {
	d, _, _ = strings.Cut(strings.TrimSpace(d), "\n")
	if d == "" {
		return ""
	}
	if r := []rune(d); len(r) > maxDescription {
		d = string(r[:maxDescription-1]) + "…"
	}
	return " — " + d
}

// ctyType writes a type from its JSON form in the schema, such as
// ["list","string"], the way it is written in HCL.
func ctyType(raw json.RawMessage) string {
	var prim string
	if json.Unmarshal(raw, &prim) == nil {
		if prim == "dynamic" {
			return "any"
		}
		return prim
	}
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 {
		return string(raw)
	}
	var kind string
	_ = json.Unmarshal(pair[0], &kind)
	switch kind {
	case "object":
		var attrs map[string]json.RawMessage
		_ = json.Unmarshal(pair[1], &attrs)
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + " = " + ctyType(attrs[k])
		}
		return "object({" + strings.Join(parts, ", ") + "})"
	case "tuple":
		var elems []json.RawMessage
		_ = json.Unmarshal(pair[1], &elems)
		parts := make([]string, len(elems))
		for i, e := range elems {
			parts[i] = ctyType(e)
		}
		return "tuple([" + strings.Join(parts, ", ") + "])"
	}
	return kind + "(" + ctyType(pair[1]) + ")"
}

// moduleDocs describes the inputs and outputs of the module call key, read
// from the module's source after init. With no key, it lists the calls.
func (b *box) moduleDocs(ctx context.Context, key string) (string, error) {
	if out, err := b.init(ctx); err != nil {
		return out, err
	}
	root, err := os.OpenRoot(b.repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	dir, err := b.relRepo(b.dir)
	if err != nil {
		return "", err
	}
	raw, err := root.ReadFile(path.Join(dir, ".terraform/modules/modules.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "The project calls no modules.", nil
	} else if err != nil {
		return "", err
	}
	var manifest struct {
		Modules []struct{ Key, Source, Version, Dir string }
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf("modules.json is not JSON: %w", err)
	}
	var keys []string
	for _, m := range manifest.Modules {
		if m.Key == "" {
			continue
		}
		from := strings.TrimSpace(m.Source + " " + m.Version)
		keys = append(keys, m.Key+" ("+from+")")
		if m.Key != key {
			continue
		}
		mdir, err := b.relRepo(filepath.Join(b.dir, filepath.FromSlash(m.Dir)))
		if err != nil {
			return "", err
		}
		files, err := tfFiles(root, mdir)
		if err != nil {
			return "", err
		}
		var w strings.Builder
		fmt.Fprintf(&w, "module %q: %s\n", m.Key, from)
		describeModule(&w, files)
		return w.String(), nil
	}
	slices.Sort(keys)
	if key == "" {
		return "Module calls: " + strings.Join(keys, ", "), nil
	}
	return "", fmt.Errorf("there is no module call %q after init; the calls are: %s", key, strings.Join(keys, ", "))
}

// relRepo returns abs as a slash path from the repository root, and
// refuses one outside it.
func (b *box) relRepo(abs string) (string, error) {
	rel, err := filepath.Rel(b.repo, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s is outside the checkout", ErrRefused, abs)
	}
	return filepath.ToSlash(rel), nil
}

type tfFile struct {
	name string
	src  []byte
	body *hclsyntax.Body
}

// tfFiles parses the .tf files of dir, a slash path under root. Files
// that do not parse are left out; validate reports them.
func tfFiles(root *os.Root, dir string) ([]tfFile, error) {
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	var out []tfFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") && !strings.HasSuffix(e.Name(), ".tofu") {
			continue
		}
		name := path.Join(dir, e.Name())
		src, err := root.ReadFile(name)
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(src, name, hcl.InitialPos)
		if diags.HasErrors() {
			continue
		}
		out = append(out, tfFile{name: e.Name(), src: src, body: f.Body.(*hclsyntax.Body)})
	}
	return out, nil
}

var spaces = regexp.MustCompile(`\s+`)

// source returns an expression as written, on one line and cut short.
func (f tfFile) source(e hclsyntax.Expression, limit int) string {
	r := e.Range()
	s := spaces.ReplaceAllString(string(f.src[r.Start.Byte:r.End.Byte]), " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

func (f tfFile) text(a *hclsyntax.Attribute) string {
	if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
		return v.AsString()
	}
	return f.source(a.Expr, 200)
}

func describeModule(w *strings.Builder, files []tfFile) {
	var required, optional, outputs []string
	for _, f := range files {
		for _, blk := range f.body.Blocks {
			if len(blk.Labels) != 1 {
				continue
			}
			attrs := blk.Body.Attributes
			desc := ""
			if a, ok := attrs["description"]; ok {
				desc = shortDescription(f.text(a))
			}
			switch blk.Type {
			case "variable":
				typ := "any"
				if a, ok := attrs["type"]; ok {
					typ = f.source(a.Expr, 200)
				}
				flags := ""
				if a, ok := attrs["sensitive"]; ok && f.source(a.Expr, 10) == "true" {
					flags = ", sensitive"
				}
				if a, ok := attrs["default"]; ok {
					optional = append(optional, fmt.Sprintf("%s: %s, default %s%s%s", blk.Labels[0], typ, f.source(a.Expr, 120), flags, desc))
				} else {
					required = append(required, fmt.Sprintf("%s: %s, required%s%s", blk.Labels[0], typ, flags, desc))
				}
			case "output":
				outputs = append(outputs, blk.Labels[0]+desc)
			}
		}
	}
	slices.Sort(required)
	slices.Sort(optional)
	slices.Sort(outputs)
	w.WriteString("Inputs:\n")
	for _, l := range append(required, optional...) {
		w.WriteString("  " + l + "\n")
	}
	w.WriteString("Outputs:\n")
	for _, l := range outputs {
		w.WriteString("  " + l + "\n")
	}
}

// conventions returns the repository's STACKWRIGHT.md files and facts
// read from the project's code, for the system prompt.
func (b *box) conventions() (string, error) {
	root, err := os.OpenRoot(b.repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	dir, err := b.relRepo(b.dir)
	if err != nil {
		return "", err
	}
	var w strings.Builder
	for _, p := range slices.Compact([]string{"STACKWRIGHT.md", path.Join(dir, "STACKWRIGHT.md")}) {
		if fi, err := root.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := root.ReadFile(p)
		if err != nil {
			continue
		}
		if len(data) > maxRules {
			data = append(data[:maxRules], "\n[cut short]"...)
		}
		fmt.Fprintf(&w, "%s:\n%s\n\n", p, strings.TrimSpace(string(data)))
	}
	facts := projectFacts(root, dir)
	if len(facts) > maxFacts {
		facts = append(facts[:maxFacts], "[more left out]")
	}
	if len(facts) > 0 {
		fmt.Fprintf(&w, "Facts read from the code in %s:\n- %s\n", dir, strings.Join(facts, "\n- "))
	}
	return w.String(), nil
}

func projectFacts(root *os.Root, dir string) []string {
	var facts []string
	if d, err := root.Open(dir); err == nil {
		entries, _ := d.ReadDir(-1)
		_ = d.Close()
		var names []string
		for _, e := range entries {
			if guard.Refused(path.Join(dir, e.Name())) != "" {
				continue
			}
			if e.IsDir() {
				names = append(names, e.Name()+"/")
			} else if strings.HasSuffix(e.Name(), ".tf") || strings.HasSuffix(e.Name(), ".tofu") || strings.HasSuffix(e.Name(), ".tfvars") {
				names = append(names, e.Name())
			}
		}
		facts = append(facts, "Files: "+strings.Join(names, ", "))
	}
	files, _ := tfFiles(root, dir)
	for _, f := range files {
		for _, blk := range f.body.Blocks {
			switch {
			case blk.Type == "terraform":
				for _, a := range sortedAttrs(blk.Body.Attributes) {
					if a.Name == "required_version" {
						facts = append(facts, "required_version = "+f.source(a.Expr, 80)+" ("+f.name+")")
					}
				}
				for _, inner := range blk.Body.Blocks {
					switch inner.Type {
					case "required_providers":
						for _, a := range sortedAttrs(inner.Body.Attributes) {
							facts = append(facts, "Provider "+a.Name+" = "+f.source(a.Expr, 160)+" ("+f.name+")")
						}
					case "backend":
						facts = append(facts, "Backend: "+strings.Join(inner.Labels, " ")+" ("+f.name+")")
					case "cloud":
						facts = append(facts, "Backend: cloud ("+f.name+")")
					}
				}
			case blk.Type == "module" && len(blk.Labels) == 1:
				m := "Module " + blk.Labels[0]
				if a, ok := blk.Body.Attributes["source"]; ok {
					m += " from " + f.source(a.Expr, 120)
				}
				if a, ok := blk.Body.Attributes["version"]; ok {
					m += " version " + f.source(a.Expr, 40)
				}
				facts = append(facts, m+" ("+f.name+")")
			}
		}
	}
	if src, err := root.ReadFile(path.Join(dir, ".terraform.lock.hcl")); err == nil {
		if lf, diags := hclsyntax.ParseConfig(src, ".terraform.lock.hcl", hcl.InitialPos); !diags.HasErrors() {
			f := tfFile{src: src, body: lf.Body.(*hclsyntax.Body)}
			for _, blk := range f.body.Blocks {
				if a, ok := blk.Body.Attributes["version"]; blk.Type == "provider" && len(blk.Labels) == 1 && ok {
					facts = append(facts, "Locked: "+blk.Labels[0]+" "+f.text(a))
				}
			}
		}
	}
	return facts
}

func sortedAttrs(attrs hclsyntax.Attributes) []*hclsyntax.Attribute {
	out := make([]*hclsyntax.Attribute, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y *hclsyntax.Attribute) int { return strings.Compare(x.Name, y.Name) })
	return out
}

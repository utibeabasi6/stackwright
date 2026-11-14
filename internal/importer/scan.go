// Package importer finds the root modules of a repository, for the import
// wizard (adr/0036). It reads files and runs no code.
package importer

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Root is a folder that the engine runs in.
type Root struct {
	// Path is the folder, "" for the repository's root.
	Path string
	// Workspaces lists "default" when it has state or no CLI workspace
	// does, then the CLI workspaces that have committed state.
	Workspaces []string
	// Backend is the backend block's type, "cloud" for a cloud block, or
	// "local".
	Backend string
	// CommittedState is true when a state file is in the repository.
	CommittedState bool
	// Engine is "opentofu" or "terraform".
	Engine string
	Slug   string
	Labels map[string]string
}

type Result struct {
	Roots []Root
	// Modules are folders that a root or a Terragrunt unit calls by a
	// local path.
	Modules []string
	// Terragrunt lists folders with a terragrunt.hcl that runs a module.
	Terragrunt []string
}

// environments are folder names that the importer proposes as env labels.
var environments = map[string]bool{"dev": true, "development": true, "test": true, "qa": true, "uat": true, "stage": true,
	"staging": true, "preprod": true, "prod": true, "production": true, "sandbox": true}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// IsScanned reports whether the scan needs a file's content, beyond its
// name.
func IsScanned(name string) bool {
	switch path.Base(name) {
	case "terragrunt.hcl", ".opentofu-version", ".terraform-version":
		return true
	}
	for _, ext := range []string{".tf", ".tofu", ".tf.json", ".tofu.json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// Scan finds the roots, modules and Terragrunt units of a repository from
// the names of all its files and the content of those IsScanned keeps.
// repo is "owner/name"; it names the root at the repository's top.
func Scan(repo string, names []string, files map[string][]byte) Result {
	hidden := func(p string) bool {
		for _, part := range strings.Split(p, "/") {
			if part == ".terraform" || part == ".git" {
				return true
			}
		}
		return false
	}
	type folder struct {
		config     []string
		terragrunt bool
		tofu       bool
	}
	folders := map[string]*folder{}
	at := func(dir string) *folder {
		if dir == "." {
			dir = ""
		}
		if folders[dir] == nil {
			folders[dir] = &folder{}
		}
		return folders[dir]
	}
	modules := map[string]bool{}
	callsFrom := func(dir string, sources []string) {
		for _, src := range sources {
			if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
				// Terragrunt's "//" separates the repository from the folder.
				modules[cleanDir(path.Join(dir, strings.Replace(src, "//", "/", 1)))] = true
			}
		}
	}
	for name, src := range files {
		if hidden(name) {
			continue
		}
		dir := path.Dir(name)
		switch base := path.Base(name); {
		case base == "terragrunt.hcl":
			unit, sources := terragrunt(src)
			if unit {
				at(dir).terragrunt = true
			}
			callsFrom(dir, sources)
		case strings.HasSuffix(base, ".tf") || strings.HasSuffix(base, ".tofu"):
			f := at(dir)
			f.config = append(f.config, name)
			f.tofu = f.tofu || strings.HasSuffix(base, ".tofu")
			callsFrom(dir, moduleSources(name, src))
		case strings.HasSuffix(base, ".tf.json") || strings.HasSuffix(base, ".tofu.json"):
			at(dir).config = append(at(dir).config, name)
		}
	}

	var r Result
	// The web app's /projects/new takes "new".
	slugs := map[string]bool{"new": true}
	for dir, f := range folders {
		switch {
		case f.terragrunt:
			r.Terragrunt = append(r.Terragrunt, dir)
			continue
		case modules[dir] || len(f.config) == 0:
			continue
		}
		root := Root{Path: dir, Backend: "local", Engine: "opentofu", Labels: map[string]string{"repo": repo}}
		for _, name := range f.config {
			if b := backendType(name, files[name]); b != "" {
				root.Backend = b
			}
		}
		prefix := ""
		if dir != "" {
			prefix = dir + "/"
			root.Labels["path"] = dir
		}
		switch {
		case f.tofu || has(names, prefix+".opentofu-version"):
		case has(names, prefix+".terraform-version") || root.Backend == "cloud" || root.Backend == "remote":
			root.Engine = "terraform"
		}
		var cli []string
		for _, n := range names {
			if ws, ok := strings.CutPrefix(n, prefix+"terraform.tfstate.d/"); ok && strings.HasSuffix(ws, "/terraform.tfstate") && strings.Count(ws, "/") == 1 {
				cli = append(cli, strings.TrimSuffix(ws, "/terraform.tfstate"))
			}
		}
		slices.Sort(cli)
		root.CommittedState = len(cli) > 0 || has(names, prefix+"terraform.tfstate")
		if has(names, prefix+"terraform.tfstate") || len(cli) == 0 {
			root.Workspaces = append(root.Workspaces, "default")
		}
		root.Workspaces = append(root.Workspaces, cli...)
		for _, part := range strings.Split(dir, "/") {
			if environments[part] {
				root.Labels["env"] = part
			}
		}
		r.Roots = append(r.Roots, root)
	}
	slices.SortFunc(r.Roots, func(a, b Root) int { return strings.Compare(a.Path, b.Path) })
	for i := range r.Roots {
		base := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(r.Roots[i].Path), "-"), "-")
		if base == "" {
			_, name, _ := strings.Cut(repo, "/")
			base = strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
		}
		if len(base) > 36 {
			base = strings.Trim(base[len(base)-36:], "-")
		}
		slug := base
		for n := 2; slugs[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		slugs[slug] = true
		r.Roots[i].Slug = slug
	}
	for m := range modules {
		if f := folders[m]; f != nil && len(f.config) > 0 {
			r.Modules = append(r.Modules, m)
		}
	}
	slices.Sort(r.Modules)
	slices.Sort(r.Terragrunt)
	return r
}

func has(names []string, name string) bool { return slices.Contains(names, name) }

func cleanDir(p string) string {
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

func parse(name string, src []byte) *hclsyntax.Body {
	f, diags := hclsyntax.ParseConfig(src, name, hcl.InitialPos)
	if diags.HasErrors() {
		return nil
	}
	return f.Body.(*hclsyntax.Body)
}

func literal(a *hclsyntax.Attribute) string {
	if a == nil {
		return ""
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

func moduleSources(name string, src []byte) []string {
	body := parse(name, src)
	if body == nil {
		return nil
	}
	var out []string
	for _, b := range body.Blocks {
		if b.Type == "module" {
			if s := literal(b.Body.Attributes["source"]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// backendType returns the type of a backend or cloud block in a file, or
// "".
func backendType(name string, src []byte) string {
	body := parse(name, src)
	if body == nil {
		return ""
	}
	for _, b := range body.Blocks {
		if b.Type != "terraform" {
			continue
		}
		for _, inner := range b.Body.Blocks {
			switch {
			case inner.Type == "backend" && len(inner.Labels) == 1:
				return inner.Labels[0]
			case inner.Type == "cloud":
				return "cloud"
			}
		}
	}
	return ""
}

// terragrunt reports whether a terragrunt.hcl runs a module itself, as a
// unit does, rather than only holding settings that units include; and
// the module sources it names.
func terragrunt(src []byte) (bool, []string) {
	body := parse("terragrunt.hcl", src)
	if body == nil {
		return false, nil
	}
	unit := false
	var sources []string
	for _, b := range body.Blocks {
		switch b.Type {
		case "include", "dependency":
			unit = true
		case "terraform":
			unit = true
			if s := literal(b.Body.Attributes["source"]); s != "" {
				sources = append(sources, s)
			}
		}
	}
	return unit, sources
}

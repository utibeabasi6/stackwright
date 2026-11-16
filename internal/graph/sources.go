package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ReadSources maps the resources, module calls and locals of the module in
// dir, and of every module that init recorded, to their files. File paths
// are relative to root. A file that does not parse is left out.
func ReadSources(root, dir string) (Sources, error) {
	var manifest struct {
		Modules []struct {
			Key string `json:"Key"`
			Dir string `json:"Dir"`
		} `json:"Modules"`
	}
	b, err := os.ReadFile(filepath.Join(dir, ".terraform", "modules", "modules.json")) //nolint:gosec // a path under the job's folder
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return Sources{}, err
	default:
		if err := json.Unmarshal(b, &manifest); err != nil {
			return Sources{}, err
		}
	}
	src := Sources{Modules: map[string]SourceModule{"": readModule(root, dir)}}
	for _, m := range manifest.Modules {
		if m.Key == "" {
			continue
		}
		mod := readModule(root, filepath.Join(dir, m.Dir))
		mod.Remote = strings.HasPrefix(filepath.ToSlash(filepath.Clean(m.Dir)), ".terraform/")
		src.Modules["module."+strings.ReplaceAll(m.Key, ".", ".module.")] = mod
	}
	return src, nil
}

func readModule(root, dir string) SourceModule {
	m := SourceModule{Resources: map[string]Location{}, ModuleCalls: map[string]Location{}, Locals: map[string][]string{}}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	tofu, _ := filepath.Glob(filepath.Join(dir, "*.tofu"))
	files = append(files, tofu...)
	for _, path := range files {
		b, err := os.ReadFile(path) //nolint:gosec // a path under the job's folder
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(b, path, hcl.InitialPos)
		body, ok := f.Body.(*hclsyntax.Body)
		if diags.HasErrors() || !ok {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		for _, block := range body.Blocks {
			loc := Location{File: filepath.ToSlash(rel), Line: block.TypeRange.Start.Line}
			switch {
			case block.Type == "resource" && len(block.Labels) == 2:
				m.Resources[block.Labels[0]+"."+block.Labels[1]] = loc
			case block.Type == "data" && len(block.Labels) == 2:
				m.Resources["data."+block.Labels[0]+"."+block.Labels[1]] = loc
			case block.Type == "module" && len(block.Labels) == 1:
				m.ModuleCalls[block.Labels[0]] = loc
			case block.Type == "locals":
				for name, attr := range block.Body.Attributes {
					var refs []string
					for _, t := range attr.Expr.Variables() {
						refs = append(refs, traversal(t))
					}
					m.Locals[name] = refs
				}
			}
		}
	}
	return m
}

// traversal writes `aws_vpc.this.id` for a reference, as the plan's JSON
// does, and stops at the first index.
func traversal(t hcl.Traversal) string {
	parts := []string{t.RootName()}
	for _, step := range t[1:] {
		attr, ok := step.(hcl.TraverseAttr)
		if !ok {
			break
		}
		parts = append(parts, attr.Name)
	}
	return strings.Join(parts, ".")
}

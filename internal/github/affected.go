package github

import (
	"path"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
	"github.com/zclconf/go-cty/cty"
)

// isConfig reports whether OpenTofu reads the file as configuration.
// https://opentofu.org/docs/language/files/
func isConfig(name string) bool {
	for _, ext := range []string{".tf", ".tofu", ".tf.json", ".tofu.json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// affected returns the projects, by id, that a change touches: a changed
// file is under the project's folder, or under a local module folder that
// it calls, directly or through other modules. changed nil means the change
// is unknown, so every project is touched. files holds the commit's
// configuration files by path.
func affected(projects map[string]string, changed []string, files map[string][]byte) map[string]bool {
	out := map[string]bool{}
	for id, folder := range projects {
		if changed == nil {
			out[id] = true
			continue
		}
		dirs := moduleDirs(path.Clean(folder), files)
		for _, f := range changed {
			for d := range dirs {
				if d == "." || strings.HasPrefix(f, d+"/") {
					out[id] = true
				}
			}
		}
	}
	return out
}

// moduleDirs returns root and every local module folder it reaches.
func moduleDirs(root string, files map[string][]byte) map[string]bool {
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		for p, src := range files {
			if path.Dir(p) != dir || !isConfig(p) {
				continue
			}
			for _, s := range localSources(p, src) {
				next := path.Join(dir, s)
				if next != ".." && !strings.HasPrefix(next, "../") {
					queue = append(queue, next)
				}
			}
		}
	}
	return seen
}

// localSources returns the source of each module block that names a local
// path. A file that does not parse adds nothing; its plan reports it.
// https://opentofu.org/docs/language/modules/sources/#local-paths
func localSources(filename string, src []byte) []string {
	var file *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(filename, ".json") {
		file, diags = hcljson.Parse(src, filename)
	} else {
		file, diags = hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	}
	if diags.HasErrors() {
		return nil
	}
	content, _, _ := file.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}}})
	var out []string
	for _, b := range content.Blocks {
		attrs, _, _ := b.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source"}}})
		attr := attrs.Attributes["source"]
		if attr == nil {
			continue
		}
		v, diags := attr.Expr.Value(nil)
		if diags.HasErrors() || v.Type() != cty.String || v.IsNull() {
			continue
		}
		if s := v.AsString(); strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
			out = append(out, s)
		}
	}
	return out
}

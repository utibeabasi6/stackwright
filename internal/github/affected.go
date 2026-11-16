package github

import (
	"path"
	"strings"

	"github.com/utibeabasi6/stackwright/internal/importer"
	"github.com/utibeabasi6/stackwright/internal/stacks"
)

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

// moduleDirs returns root and every local module folder it reaches. A
// Terragrunt project's units sit in folders under root, and each one's
// terragrunt.hcl names its module.
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
			unit := dir == root && path.Base(p) == "terragrunt.hcl" && (root == "." || strings.HasPrefix(p, root+"/"))
			if !unit && (path.Dir(p) != dir || !stacks.IsConfig(p)) {
				continue
			}
			for _, s := range importer.LocalSources(p, src) {
				next := path.Join(path.Dir(p), s)
				if next != ".." && !strings.HasPrefix(next, "../") {
					queue = append(queue, next)
				}
			}
		}
	}
	return seen
}

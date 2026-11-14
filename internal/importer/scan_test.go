package importer

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// read loads a fixture as the scan sees a repository archive: every
// file's name, and the content of those it needs.
func read(t *testing.T, layout string) ([]string, map[string][]byte) {
	t.Helper()
	root := filepath.Join("testdata", layout)
	var names []string
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		names = append(names, rel)
		if IsScanned(rel) {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names, files
}

func TestScanFindsEachFixtureLayout(t *testing.T) {
	labels := func(kv ...string) map[string]string {
		m := map[string]string{"repo": "acme/infra"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tc := range []struct {
		layout string
		want   Result
	}{
		{"plain", Result{Roots: []Root{
			{Path: "", Workspaces: []string{"default"}, Backend: "s3", Engine: "opentofu", Slug: "infra", Labels: labels()},
		}}},
		{"envs", Result{
			Roots: []Root{
				{Path: "envs/dev", Workspaces: []string{"default"}, Backend: "s3", Engine: "opentofu", Slug: "envs-dev",
					Labels: labels("path", "envs/dev", "env", "dev")},
				{Path: "envs/prod", Workspaces: []string{"default"}, Backend: "s3", Engine: "opentofu", Slug: "envs-prod",
					Labels: labels("path", "envs/prod", "env", "prod")},
			},
			Modules: []string{"modules/network"},
		}},
		{"cli-workspaces", Result{Roots: []Root{
			{Path: "", Workspaces: []string{"prod", "staging"}, Backend: "local", CommittedState: true, Engine: "opentofu", Slug: "infra",
				Labels: labels()},
		}}},
		{"committed", Result{Roots: []Root{
			{Path: "infra", Workspaces: []string{"default"}, Backend: "local", CommittedState: true, Engine: "opentofu", Slug: "infra",
				Labels: labels("path", "infra")},
		}}},
		{"terragrunt", Result{
			Roots: []Root{
				{Path: "live", Workspaces: []string{"default"}, Backend: "terragrunt", Engine: "opentofu", Slug: "live", Labels: labels("path", "live"),
					Units: []string{"live/prod/vpc"}},
			},
			Modules:    []string{"modules/vpc"},
			Terragrunt: []string{"live/prod/vpc"},
		}},
		{"engines", Result{Roots: []Root{
			{Path: "apps/api", Workspaces: []string{"default"}, Backend: "local", Engine: "opentofu", Slug: "apps-api", Labels: labels("path", "apps/api")},
			{Path: "apps/batch", Workspaces: []string{"default"}, Backend: "local", Engine: "terraform", Slug: "apps-batch",
				Labels: labels("path", "apps/batch")},
			{Path: "apps/web", Workspaces: []string{"default"}, Backend: "cloud", Engine: "terraform", Slug: "apps-web", Labels: labels("path", "apps/web")},
		}}},
		{"stack", Result{
			Roots: []Root{
				{Path: "stack", Workspaces: []string{}, Backend: "stackwright", Engine: "opentofu", Slug: "stack", Labels: labels("path", "stack"),
					StackDeployments: []string{"dev", "prod"}},
			},
			Modules: []string{"shared/nulls", "stack/pet"},
		}},
		{"nested", Result{
			Roots: []Root{
				{Path: "stacks/api", Workspaces: []string{"default"}, Backend: "local", Engine: "opentofu", Slug: "stacks-api",
					Labels: labels("path", "stacks/api")},
			},
			Modules: []string{"modules/bucket", "modules/service"},
		}},
	} {
		t.Run(tc.layout, func(t *testing.T) {
			names, files := read(t, tc.layout)
			if got := Scan("acme/infra", names, files); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestScanGivesEachRootItsOwnSlug(t *testing.T) {
	files := map[string][]byte{"a-b/main.tf": nil, "a/b/main.tf": nil}
	got := Scan("acme/infra", []string{"a-b/main.tf", "a/b/main.tf"}, files)
	if len(got.Roots) != 2 || got.Roots[0].Slug != "a-b" || got.Roots[1].Slug != "a-b-2" {
		t.Errorf("slugs: %+v", got.Roots)
	}
	if got := Scan("acme/infra", []string{"new/main.tf"}, map[string][]byte{"new/main.tf": nil}); got.Roots[0].Slug != "new-2" {
		t.Errorf("a folder named new: %+v", got.Roots)
	}
}

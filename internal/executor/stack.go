package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/stacks"
)

// stackComponents is the job variable that holds the outputs of the
// components an instance reads (adr/0043).
const stackComponents = "stackwright_components"

// stack writes a Stack instance's root module and values into dir, the
// Stack's folder, and returns the environment entries of its ephemeral
// values (adr/0043).
func (r *runner) stack(root, dir string) ([]string, error) {
	job, st := r.job, r.job.GetStack()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir() || strings.HasPrefix(name, "stackwright_"):
		case stacks.IsStackFile(name):
			if files[name], err = os.ReadFile(filepath.Join(dir, name)); err != nil { //nolint:gosec // a file of the source tree
				return nil, err
			}
		case strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tofu") || strings.HasSuffix(name, ".tf.json"):
			return nil, fmt.Errorf("the Stack's folder has its own configuration file %s; Stackwright writes the root module there", name)
		}
	}
	cfg, err := stacks.Parse(files)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(cfg.Refused...); err != nil {
		return nil, err
	}
	// Ephemeral input variables came in OpenTofu 1.11 and Terraform 1.10.
	parts := strings.SplitN(job.GetEngineVersion(), ".", 3)
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	want := 10
	if job.GetEngine() == v1.Engine_ENGINE_OPENTOFU {
		want = 11
	}
	ephemeral := major > 1 || major == 1 && minor >= want
	for _, v := range cfg.Variables {
		if v.Ephemeral && !ephemeral {
			return nil, fmt.Errorf("variable %q is ephemeral, which needs OpenTofu 1.11 or Terraform 1.10; the project runs %s", v.Name,
				job.GetEngineVersion())
		}
	}
	components, sensitive := json.RawMessage("{}"), false
	for _, v := range job.GetVariables() {
		if v.GetKey() == stackComponents {
			components, sensitive = json.RawMessage(v.GetValue()), v.GetSensitive()
		}
	}
	for _, t := range st.GetIdentityTokens() {
		r.secrets.Add(t)
	}
	vals, err := cfg.Values(st.GetDeployment(), stacks.Env{Tokens: st.GetIdentityTokens(), Store: func(category, key string) (string, bool) {
		want := v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM
		if category == "env" {
			want = v1.VariableCategory_VARIABLE_CATEGORY_ENV
		}
		for _, v := range job.GetVariables() {
			if v.GetCategory() == want && v.GetKey() == key {
				return v.GetValue(), true
			}
		}
		return "", false
	}})
	if err != nil {
		return nil, err
	}
	inst, err := cfg.Find(st.GetDeployment(), st.GetComponent(), st.GetKey(), st.GetKeyed(), vals)
	if err != nil {
		return nil, err
	}
	var mod stacks.Module
	if inst.Component != "" {
		rel, err := filepath.Rel(root, filepath.Join(dir, cfg.SourceOf(inst)))
		if err != nil {
			return nil, err
		}
		src, err := within(root, filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		if mod, err = stacks.ReadModule(src); err != nil {
			return nil, fmt.Errorf("component %s: %w", inst.Component, err)
		}
	}
	g, err := cfg.Generate(inst, vals, components, sensitive, mod)
	if err != nil {
		return nil, err
	}
	for name, b := range map[string][]byte{"stackwright_stack.tf": g.Main, "stackwright_stack.auto.tfvars.json": g.Vars} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	for _, e := range g.Env {
		_, value, _ := strings.Cut(e, "=")
		r.secrets.Add(value)
	}
	return g.Env, nil
}

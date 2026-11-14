package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

// terragruntBin is the Terragrunt in the executor image (adr/0041).
const terragruntBin = "/usr/local/bin/terragrunt"

var notName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

type unit struct {
	Type         string   `json:"type"`
	Path         string   `json:"path"`
	Dependencies []string `json:"dependencies"`
	slug         string
}

// stackUnits lists the units of the stack in dir in dependency order, each
// with a slug that names its module in the merged plan.
func stackUnits(ctx context.Context, dir string, env []string) ([]unit, error) {
	cmd := exec.CommandContext(ctx, terragruntBin, "find", "--dag", "--json", "--dependencies")
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terragrunt find: %w: %s", err, stderr.Bytes())
	}
	var all []unit
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("terragrunt find: %w", err)
	}
	var units []unit
	taken := map[string]bool{}
	for _, u := range all {
		if u.Type != "unit" {
			continue
		}
		base := strings.Trim(notName.ReplaceAllString(u.Path, "_"), "_")
		if base == "" || base[0] >= '0' && base[0] <= '9' {
			base = "unit_" + base
		}
		u.slug = base
		for n := 2; taken[u.slug]; n++ {
			u.slug = fmt.Sprintf("%s_%d", base, n)
		}
		taken[u.slug] = true
		units = append(units, u)
	}
	if len(units) == 0 {
		return nil, errors.New("the folder has no Terragrunt units")
	}
	return units, nil
}

// terragrunt plans or applies a stack's units in dependency order. A
// plan uploads the units' plan files as plan.bin, and one plan.json with
// each unit as a module (adr/0041).
func (r *runner) terragrunt(ctx context.Context, tofu, dir, root string, env []string, planTar []byte) error {
	env = append(env, "TG_NON_INTERACTIVE=true", "TG_NO_COLOR=true")
	units, err := stackUnits(ctx, dir, env)
	if err != nil {
		return &stageError{"units", err}
	}
	paths := make([]string, len(units))
	for i, u := range units {
		paths[i] = u.Path
	}
	r.emit("executor.units", map[string]any{"units": paths})
	plans := filepath.Join(r.work, "plans")
	if err := os.MkdirAll(plans, 0o700); err != nil {
		return &stageError{"source", err}
	}
	var planFlags []string
	if r.job.GetRefreshOnly() {
		planFlags = append(planFlags, "-refresh-only")
	}
	engine := func(u unit) *cli {
		return &cli{bin: terragruntBin, prefix: []string{"run", "--tf-path", tofu, "--"}, dir: filepath.Join(dir, u.Path), env: env, planFlags: planFlags,
			redact: r.redact, line: func(e event) {
				if e.resourceAddr != "" {
					e.resourceAddr = "module." + u.slug + "." + e.resourceAddr
				}
				r.log.Write(e.payload)
				r.log.WriteByte('\n')
				r.rep.emit(e.typ, e.resourceAddr, e.payload)
			}}
	}

	if r.job.GetOperation() == v1.JobOperation_JOB_OPERATION_APPLY {
		if err := untar(planTar, plans); err != nil {
			return &stageError{"inputs", err}
		}
		for _, u := range units {
			plan := filepath.Join(plans, u.slug+".bin")
			if _, err := os.Stat(plan); errors.Is(err, os.ErrNotExist) {
				continue
			}
			r.emit("executor.unit_started", map[string]any{"unit": u.Path})
			if err := engine(u).apply(ctx, plan); err != nil {
				return &stageError{"apply", fmt.Errorf("%s: %w", u.Path, err)}
			}
		}
		return nil
	}

	var parts []unitPlan
	for _, u := range units {
		r.emit("executor.unit_started", map[string]any{"unit": u.Path})
		plan := filepath.Join(plans, u.slug+".bin")
		e := engine(u)
		changes, err := e.plan(ctx, plan)
		if err != nil {
			return &stageError{"plan", fmt.Errorf("%s: %w", u.Path, err)}
		}
		// OpenTofu shows the plan in the folder Terragrunt ran it in: show
		// through Terragrunt would read dependencies' outputs again, which a
		// unit may mock for plan only.
		work := filepath.Join(dir, u.Path)
		if !isDir(filepath.Join(work, ".terraform")) {
			err := filepath.WalkDir(filepath.Join(work, ".terragrunt-cache"), func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.IsDir() || d.Name() != ".terraform" {
					return err
				}
				work = filepath.Dir(p)
				return fs.SkipAll
			})
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return &stageError{"plan", err}
			}
		}
		js, err := (&cli{bin: tofu, dir: work, env: env, redact: r.redact}).show(ctx, plan)
		if err != nil {
			return &stageError{"plan", fmt.Errorf("%s: %w", u.Path, err)}
		}
		parts = append(parts, unitPlan{slug: u.slug, path: u.Path, plan: js})
		if changes {
			r.hasChanges = true
		} else if err := os.Remove(plan); err != nil {
			return &stageError{"plan", err}
		}
	}
	planJSON, err := mergePlans(parts)
	if err != nil {
		return &stageError{"plan", err}
	}
	tarred, err := tarTree(plans, "")
	if err != nil {
		return &stageError{"artifacts", err}
	}
	if err := r.upload(ctx, "plan.bin", tarred); err != nil {
		return &stageError{"artifacts", err}
	}
	return r.finishPlan(ctx, planJSON, root, dir, "")
}

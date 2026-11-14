package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/stacks"
)

// ErrStackInputs is a run of a Stack's component instance that reads
// outputs no apply has made yet.
var ErrStackInputs = errors.New("the components it reads have not applied")

var (
	workspaceName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,38}[a-z0-9])?$`)
	notDeployment  = regexp.MustCompile(`[^a-z0-9-]+`)
	deploymentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
)

// StackDeployment is a deployment that a Stack's files define, and whether
// they ask to destroy it.
type StackDeployment struct {
	ID, Name string
	Destroy  bool
}

// SyncStack makes a Stack project's workspaces and deployments match its
// files, the files at the top of the Stack's folder (adr/0043). A sync
// that fails changes nothing but the project's stack_error. tx needs the
// org setting.
func SyncStack(ctx context.Context, tx pgx.Tx, orgID, projectID string, files map[string][]byte) ([]StackDeployment, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	out, syncErr := syncStack(ctx, sp, orgID, projectID, files)
	if syncErr != nil {
		err = sp.Rollback(ctx)
	} else {
		err = sp.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	msg := ""
	if syncErr != nil {
		msg = syncErr.Error()
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET stack_error = nullif($2, '') WHERE id = $1`, projectID, msg); err != nil {
		return nil, err
	}
	return out, syncErr
}

type priorInstance struct {
	workspace string
	stacks.Prior
	removed bool
}

func syncStack(ctx context.Context, tx pgx.Tx, orgID, projectID string, files map[string][]byte) ([]StackDeployment, error) {
	cfg, err := stacks.Parse(files)
	if err != nil {
		return nil, err
	}
	audiences, err := cfg.Audiences()
	if err != nil {
		return nil, err
	}
	var slug string
	if err := tx.QueryRow(ctx, `SELECT slug FROM projects WHERE id = $1`, projectID).Scan(&slug); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT workspace_id, deployment, component, instance_key, keyed, removed FROM stack_instances
		WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct {
		deployment string
		priorInstance
	}, error) {
		var x struct {
			deployment string
			priorInstance
		}
		err := r.Scan(&x.workspace, &x.deployment, &x.Component, &x.Key, &x.Keyed, &x.removed)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	prior := map[string][]priorInstance{}
	for _, x := range all {
		prior[x.deployment] = append(prior[x.deployment], x.priorInstance)
	}

	names := map[string]string{}
	var out []StackDeployment
	for _, d := range cfg.Deployments {
		vals, err := cfg.Values(d.Name, stacks.Env{})
		if err != nil {
			return nil, err
		}
		var before []stacks.Prior
		for _, p := range prior[d.Name] {
			if p.Component != "" {
				before = append(before, p.Prior)
			}
		}
		instances, err := cfg.Instances(d.Name, vals, before)
		if err != nil {
			return nil, err
		}
		type step struct {
			workspace string
			inst      stacks.Instance
			needs     []string
		}
		var steps []step
		for _, inst := range instances {
			name := inst.Workspace()
			if !workspaceName.MatchString(name) {
				return nil, fmt.Errorf("deployment %q: the workspace name %q is not valid; names have 40 characters at most", d.Name, name)
			}
			if other, ok := names[name]; ok {
				return nil, fmt.Errorf("deployment %q: %s and %s would both be workspace %q", d.Name, other, describe(inst), name)
			}
			names[name] = describe(inst)
			i := slices.IndexFunc(prior[d.Name], func(p priorInstance) bool {
				return p.Component == inst.Component && p.Key == inst.Key
			})
			id := ""
			if i >= 0 {
				id = prior[d.Name][i].workspace
			}
			applied, destroy, err := lastApply(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if inst.Removed && (!inst.Destroy || applied && destroy) {
				if _, err := tx.Exec(ctx, `UPDATE workspaces SET archived_at = now() WHERE id = $1`, id); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM stack_instances WHERE workspace_id = $1`, id); err != nil {
					return nil, err
				}
				continue
			}
			if id == "" {
				id = uuid.NewString()
				var taken bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM workspaces WHERE project_id = $1 AND name = $2 AND archived_at IS NULL)`,
					projectID, name).Scan(&taken); err != nil {
					return nil, err
				}
				if taken {
					return nil, fmt.Errorf("deployment %q: workspace %q exists and is not %s", d.Name, name, describe(inst))
				}
				if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id, org_id, project_id, name) VALUES ($1, $2, $3, $4)`,
					id, orgID, projectID, name); err != nil {
					return nil, err
				}
			}
			var needs []string
			switch {
			case inst.Removed:
			case inst.Component == "":
				needs = cfg.OutputsNeed()
			default:
				needs = cfg.Needs(inst.Component)
			}
			keyed := map[string]bool{}
			for _, n := range needs {
				keyed[n] = cfg.Keyed(n)
			}
			needsJSON, _ := json.Marshal(keyed)
			tokensJSON, _ := json.Marshal(audiences)
			if _, err := tx.Exec(ctx, `INSERT INTO stack_instances (workspace_id, org_id, project_id, deployment, component, instance_key, keyed,
				removed, needs, identity_tokens) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (workspace_id) DO UPDATE SET removed = EXCLUDED.removed, needs = EXCLUDED.needs,
				identity_tokens = EXCLUDED.identity_tokens`,
				id, orgID, projectID, d.Name, inst.Component, inst.Key, inst.Keyed, inst.Removed, needsJSON, tokensJSON); err != nil {
				return nil, err
			}
			steps = append(steps, step{workspace: id, inst: inst, needs: needs})
		}

		var depID string
		err = tx.QueryRow(ctx, `SELECT id FROM deployments WHERE project_id = $1 AND stack_deployment = $2 AND archived_at IS NULL`,
			projectID, d.Name).Scan(&depID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			depID = uuid.NewString()
			name := strings.Trim(notDeployment.ReplaceAllString(strings.ToLower(slug+"-"+d.Name), "-"), "-")
			if !deploymentName.MatchString(name) {
				return nil, fmt.Errorf("deployment %q: the name %q is not valid for a deployment; names have 40 characters at most", d.Name, name)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO deployments (id, org_id, name, project_id, stack_deployment, stack_group)
				VALUES ($1, $2, $3, $4, $5, nullif($6, ''))`, depID, orgID, name, projectID, d.Name, d.Group); err != nil {
				return nil, fmt.Errorf("deployment %q as %q: %w", d.Name, name, err)
			}
		case err != nil:
			return nil, err
		default:
			if _, err := tx.Exec(ctx, `UPDATE deployments SET stack_group = nullif($2, '') WHERE id = $1`, depID, d.Group); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM deployment_steps WHERE deployment_id = $1`, depID); err != nil {
			return nil, err
		}
		for _, s := range steps {
			if _, err := tx.Exec(ctx, `INSERT INTO deployment_steps (deployment_id, org_id, workspace_id, destroy) VALUES ($1, $2, $3, $4)`,
				depID, orgID, s.workspace, s.inst.Removed); err != nil {
				return nil, err
			}
		}
		for _, to := range steps {
			for _, from := range steps {
				if from.inst.Removed || from.workspace == to.workspace {
					continue
				}
				if to.inst.Removed || slices.Contains(to.needs, from.inst.Component) && from.inst.Component != "" {
					if _, err := tx.Exec(ctx, `INSERT INTO deployment_links (deployment_id, org_id, from_workspace_id, to_workspace_id)
						VALUES ($1, $2, $3, $4)`, depID, orgID, from.workspace, to.workspace); err != nil {
						return nil, err
					}
				}
			}
		}
		out = append(out, StackDeployment{ID: depID, Name: d.Name, Destroy: d.Destroy})
	}

	for name, instances := range prior {
		if slices.ContainsFunc(cfg.Deployments, func(d *stacks.Deployment) bool { return d.Name == name }) {
			continue
		}
		for _, p := range instances {
			applied, destroy, err := lastApply(ctx, tx, p.workspace)
			if err != nil {
				return nil, err
			}
			if applied && !destroy {
				return nil, fmt.Errorf("deployment %q is gone from the files while its workspaces hold resources; set destroy = true in it, deploy, then remove it", name)
			}
		}
		for _, p := range instances {
			if _, err := tx.Exec(ctx, `UPDATE workspaces SET archived_at = now() WHERE id = $1`, p.workspace); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM stack_instances WHERE workspace_id = $1`, p.workspace); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE deployments SET archived_at = now() WHERE project_id = $1 AND stack_deployment = $2
			AND archived_at IS NULL`, projectID, name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func describe(i stacks.Instance) string {
	switch {
	case i.Component == "":
		return "the stack outputs"
	case i.Keyed:
		return fmt.Sprintf("component %s[%q]", i.Component, i.Key)
	}
	return "component " + i.Component
}

// lastApply reports whether a workspace applied, and whether its last
// apply was a destroy.
func lastApply(ctx context.Context, tx pgx.Tx, workspaceID string) (applied, destroy bool, err error) {
	if workspaceID == "" {
		return false, false, nil
	}
	err = tx.QueryRow(ctx, `SELECT destroy FROM runs WHERE workspace_id = $1 AND status = 'applied' ORDER BY number DESC LIMIT 1`,
		workspaceID).Scan(&destroy)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, destroy, err
}

// stackInputs are the variables of a Stack's component instance: one,
// stackwright_components, with the newest outputs of the components it
// reads (adr/0043). It returns the workspaces whose outputs are missing,
// and false for a workspace that is not a Stack's.
func (o *Orchestrator) stackInputs(ctx context.Context, tx pgx.Tx, orgID, workspaceID string) ([]byte, []string, bool, error) {
	var projectID, deployment string
	var needs map[string]bool
	err := tx.QueryRow(ctx, `SELECT project_id, deployment, needs FROM stack_instances WHERE workspace_id = $1`, workspaceID).
		Scan(&projectID, &deployment, &needs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, true, err
	}
	components := map[string]any{}
	var names []string
	for name, keyed := range needs {
		names = append(names, name)
		if keyed {
			components[name] = map[string]any{}
		}
	}
	rows, err := tx.Query(ctx, `SELECT w.name, si.component, si.instance_key, si.keyed, wo.outputs FROM stack_instances si
		JOIN workspaces w ON w.id = si.workspace_id LEFT JOIN workspace_outputs wo ON wo.workspace_id = si.workspace_id
		WHERE si.project_id = $1 AND si.deployment = $2 AND NOT si.removed AND si.component = ANY($3) AND w.archived_at IS NULL
		ORDER BY w.name`, projectID, deployment, names)
	if err != nil {
		return nil, nil, true, err
	}
	type row struct {
		name, component, key string
		keyed                bool
		sealed               []byte
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.name, &x.component, &x.key, &x.keyed, &x.sealed)
	})
	if err != nil {
		return nil, nil, true, err
	}
	var missing []string
	sensitive := false
	for _, x := range all {
		if x.sealed == nil {
			missing = append(missing, x.name)
			continue
		}
		plain, err := o.KEK.Open(ctx, tx, orgID, x.sealed)
		if err != nil {
			return nil, nil, true, err
		}
		var outs map[string]output
		if err := json.Unmarshal(plain, &outs); err != nil {
			return nil, nil, true, fmt.Errorf("the outputs of %s: %w", x.name, err)
		}
		vals := map[string]json.RawMessage{}
		for k, out := range outs {
			vals[k] = out.Value
			sensitive = sensitive || out.Sensitive
		}
		if x.keyed {
			components[x.component].(map[string]any)[x.key] = vals
		} else {
			components[x.component] = vals
		}
	}
	for _, name := range names {
		if _, ok := components[name]; !ok && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	value, err := json.Marshal(components)
	if err != nil {
		return nil, nil, true, err
	}
	b, err := json.Marshal([]*v1.JobVariable{{Category: v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM, Key: "stackwright_components",
		Value: string(value), Sensitive: sensitive}})
	return b, missing, true, err
}

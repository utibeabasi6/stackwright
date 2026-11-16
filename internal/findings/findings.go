// Package findings turns Trivy's report on a plan into findings, keyed by
// workspace, check and resource, and says which block an apply
// (adr/0037).
package findings

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // reproduces Trivy's resource names; no security use
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/utibeabasi6/stackwright/internal/notify"
)

// Result is one failed check on one resource.
type Result struct {
	CheckID  string
	Severity string
	Title    string
	Message  string
	Resource string
	// Unmatched is a resource that no plan address fits; it never blocks
	// an apply (adr/0040).
	Unmatched bool
}

// Blocks reports whether a severity, as stored, can block an apply.
func Blocks(severity string) bool { return severity == "high" || severity == "critical" }

// Parse reads `trivy config --format json`. Only failed checks count, once
// for each check and resource.
func Parse(raw []byte) ([]Result, error) {
	var report struct {
		Results []struct {
			Misconfigurations []struct {
				ID, Title, Message, Severity, Status string
				CauseMetadata                        struct{ Resource string }
			}
		}
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("the scan report is not JSON: %w", err)
	}
	seen := map[string]bool{}
	var out []Result
	for _, r := range report.Results {
		for _, m := range r.Misconfigurations {
			if m.Status != "FAIL" || m.CauseMetadata.Resource == "" {
				continue
			}
			key := m.ID + "\x00" + m.CauseMetadata.Resource
			if seen[key] {
				continue
			}
			seen[key] = true
			severity := strings.ToLower(m.Severity)
			if !slices.Contains([]string{"low", "medium", "high", "critical"}, severity) {
				severity = "low"
			}
			out = append(out, Result{CheckID: m.ID, Severity: severity, Title: m.Title, Message: m.Message, Resource: m.CauseMetadata.Resource})
		}
	}
	return out, nil
}

// Match gives each result its plan address, and returns the addresses the
// plan creates, updates or replaces. Trivy 0.76.1 names a resource without
// its own count or for_each key, and a resource in a module as
// <type>.<name>_<md5 of the plan's module_address>, as
// aws_security_group.this_b9757342... for module.web (adr/0040). A
// result that fits no address keeps Trivy's name and is unmatched.
func Match(results []Result, planJSON []byte) ([]Result, map[string]bool, error) {
	var plan struct {
		ResourceChanges []struct {
			ModuleAddress string `json:"module_address"`
			Mode          string `json:"mode"`
			Type          string `json:"type"`
			Name          string `json:"name"`
			Change        struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, nil, fmt.Errorf("the plan is not JSON: %w", err)
	}
	byTrivy, changed := map[string]string{}, map[string]bool{}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		addr, name := rc.Type+"."+rc.Name, rc.Type+"."+rc.Name
		if rc.ModuleAddress != "" {
			addr = rc.ModuleAddress + "." + addr
			name = fmt.Sprintf("%s_%x", name, md5.Sum([]byte(rc.ModuleAddress))) //nolint:gosec // as Trivy names it
		}
		byTrivy[name] = addr
		if slices.Contains(rc.Change.Actions, "create") || slices.Contains(rc.Change.Actions, "update") {
			changed[addr] = true
		}
	}
	out := make([]Result, len(results))
	for i, r := range results {
		if addr, ok := byTrivy[r.Resource]; ok {
			r.Resource = addr
		} else {
			r.Unmatched = true
		}
		out[i] = r
	}
	return out, changed, nil
}

// Record opens the findings a plan of the workspace's own code saw, after
// Match, and marks fixed those it no longer sees. tx needs the org setting.
func Record(ctx context.Context, tx pgx.Tx, orgID, workspaceID string, results []Result) error {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		var id string
		var opened bool
		if err := tx.QueryRow(ctx, `INSERT INTO findings (id, org_id, workspace_id, check_id, resource, severity, title, message, unmatched)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (workspace_id, check_id, resource) DO UPDATE SET severity = $6, title = $7, message = $8, unmatched = $9,
				last_seen_at = now(), fixed_at = NULL
			RETURNING id, xmax = 0`, uuid.NewString(), orgID, workspaceID, r.CheckID, r.Resource, r.Severity, r.Title, r.Message,
			r.Unmatched).Scan(&id, &opened); err != nil {
			return err
		}
		ids = append(ids, id)
		if opened && Blocks(r.Severity) {
			e := notify.Event{Type: notify.FindingOpened, Resource: r.Resource, Severity: r.Severity, Check: r.CheckID}
			if err := workspaceOf(ctx, tx, workspaceID, &e); err != nil {
				return err
			}
			e.Text = fmt.Sprintf("! %s finding in %s/%s: %s on %s", strings.ToUpper(r.Severity[:1])+r.Severity[1:], e.Project, e.Workspace,
				r.Title, r.Resource)
			if err := notify.Enqueue(ctx, tx, orgID, e); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `UPDATE findings SET fixed_at = now() WHERE workspace_id = $1 AND fixed_at IS NULL AND check_id <> $3
		AND NOT (id = ANY($2::uuid[]))`, workspaceID, ids, DriftCheck)
	return err
}

// DriftCheck is the check id of a drift finding (adr/0042).
const DriftCheck = "DRIFT"

// Drift reads a refresh-only plan's resource_drift: each resource that
// changed outside Stackwright, with the attributes that changed.
// https://opentofu.org/docs/internals/json-format/#plan-representation
func Drift(planJSON []byte) ([]Result, error) {
	var plan struct {
		ResourceDrift []struct {
			Address string `json:"address"`
			Mode    string `json:"mode"`
			Change  struct {
				Actions []string                   `json:"actions"`
				Before  map[string]json.RawMessage `json:"before"`
				After   map[string]json.RawMessage `json:"after"`
			} `json:"change"`
		} `json:"resource_drift"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, fmt.Errorf("the plan is not JSON: %w", err)
	}
	var out []Result
	for _, d := range plan.ResourceDrift {
		if d.Mode != "managed" {
			continue
		}
		r := Result{CheckID: DriftCheck, Severity: "medium", Title: "Changed outside Stackwright", Resource: d.Address}
		if slices.Contains(d.Change.Actions, "delete") {
			r.Message = "Deleted outside Stackwright."
		} else {
			var keys []string
			for k, v := range d.Change.After {
				if !bytes.Equal(v, d.Change.Before[k]) {
					keys = append(keys, k)
				}
			}
			for k := range d.Change.Before {
				if _, ok := d.Change.After[k]; !ok {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			r.Message = "Changed: " + strings.Join(keys, ", ") + "."
		}
		out = append(out, r)
	}
	return out, nil
}

// RecordDrift opens a drift finding for each drifted resource, and closes
// the workspace's drift findings that a check no longer sees. A drift
// finding is Medium, so it never blocks an apply. tx needs the org setting.
func RecordDrift(ctx context.Context, tx pgx.Tx, orgID, workspaceID string, drift []Result) error {
	ids := make([]string, 0, len(drift))
	var opened []string
	for _, r := range drift {
		var id string
		var isNew bool
		if err := tx.QueryRow(ctx, `INSERT INTO findings (id, org_id, workspace_id, check_id, resource, severity, title, message)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (workspace_id, check_id, resource) DO UPDATE SET message = $8, last_seen_at = now(), fixed_at = NULL
			RETURNING id, xmax = 0`, uuid.NewString(), orgID, workspaceID, r.CheckID, r.Resource, r.Severity, r.Title, r.Message).Scan(&id, &isNew); err != nil {
			return err
		}
		ids = append(ids, id)
		if isNew {
			opened = append(opened, r.Resource)
		}
	}
	if len(opened) > 0 {
		e := notify.Event{Type: notify.DriftFound, Resource: strings.Join(opened, ", ")}
		if err := workspaceOf(ctx, tx, workspaceID, &e); err != nil {
			return err
		}
		e.Text = fmt.Sprintf("~ Drift in %s/%s: %s changed outside Stackwright", e.Project, e.Workspace, e.Resource)
		if err := notify.Enqueue(ctx, tx, orgID, e); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE findings SET fixed_at = now() WHERE workspace_id = $1 AND fixed_at IS NULL AND check_id = $3
		AND NOT (id = ANY($2::uuid[]))`, workspaceID, ids, DriftCheck)
	return err
}

// Blocking returns the open findings of a workspace that block a plan
// changing those addresses: High or Critical, with no accepted risk in
// force. tx needs the org setting.
func Blocking(ctx context.Context, tx pgx.Tx, workspaceID string, changed map[string]bool) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT f.id, f.resource FROM findings f
		WHERE f.workspace_id = $1 AND f.fixed_at IS NULL AND f.severity IN ('high', 'critical')
		AND NOT EXISTS (SELECT FROM risk_acceptances a WHERE a.finding_id = f.id AND a.expires_at > now())`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, resource string
		if err := rows.Scan(&id, &resource); err != nil {
			return nil, err
		}
		if changed[resource] {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// workspaceOf names an event's workspace.
func workspaceOf(ctx context.Context, tx pgx.Tx, workspaceID string, e *notify.Event) error {
	return tx.QueryRow(ctx, `SELECT o.slug, p.slug, w.name FROM workspaces w JOIN projects p ON p.id = w.project_id
		JOIN orgs o ON o.id = w.org_id WHERE w.id = $1`, workspaceID).Scan(&e.Org, &e.Project, &e.Workspace)
}

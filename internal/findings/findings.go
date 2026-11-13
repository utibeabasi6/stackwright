// Package findings turns Trivy's report on a plan into findings, keyed by
// workspace, check and resource, and says which block an apply
// (adr/0037).
package findings

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Result is one failed check on one resource.
type Result struct {
	CheckID  string
	Severity string
	Title    string
	Message  string
	Resource string
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

// Changed returns the addresses that a plan creates, updates or replaces.
func Changed(planJSON []byte) (map[string]bool, error) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, fmt.Errorf("the plan is not JSON: %w", err)
	}
	out := map[string]bool{}
	for _, rc := range plan.ResourceChanges {
		if slices.Contains(rc.Change.Actions, "create") || slices.Contains(rc.Change.Actions, "update") {
			out[rc.Address] = true
		}
	}
	return out, nil
}

// Record opens the findings a plan of the workspace's own code saw, and
// marks fixed those it no longer sees. tx needs the org setting.
func Record(ctx context.Context, tx pgx.Tx, orgID, workspaceID string, results []Result) error {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO findings (id, org_id, workspace_id, check_id, resource, severity, title, message)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (workspace_id, check_id, resource) DO UPDATE SET severity = $6, title = $7, message = $8,
				last_seen_at = now(), fixed_at = NULL
			RETURNING id`, uuid.NewString(), orgID, workspaceID, r.CheckID, r.Resource, r.Severity, r.Title, r.Message).Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	_, err := tx.Exec(ctx, `UPDATE findings SET fixed_at = now() WHERE workspace_id = $1 AND fixed_at IS NULL AND NOT (id = ANY($2::uuid[]))`,
		workspaceID, ids)
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

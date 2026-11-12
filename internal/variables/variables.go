// Package variables reads the variables that a workspace's runs get.
package variables

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Row is one stored variable. Value is nil for a sensitive one, which
// Sealed holds instead.
type Row struct {
	Category    string
	Key         string
	Sensitive   bool
	Value       *string
	Sealed      []byte
	Description string
	Project     string
	Workspace   string
	UpdatedAt   time.Time
	UpdatedBy   string
}

const columns = `v.category, v.key, v.sensitive, v.value, v.value_sealed, v.description,
	coalesce(p.slug, wp.slug, ''), coalesce(w.name, ''), v.updated_at, coalesce(nullif(u.name, ''), u.email, '')
	FROM variables v
	LEFT JOIN projects p ON p.id = v.project_id
	LEFT JOIN workspaces w ON w.id = v.workspace_id
	LEFT JOIN projects wp ON wp.id = w.project_id
	LEFT JOIN users u ON u.id = v.updated_by`

func collect(rows pgx.Rows) ([]Row, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Row, error) {
		var v Row
		return v, r.Scan(&v.Category, &v.Key, &v.Sensitive, &v.Value, &v.Sealed, &v.Description, &v.Project, &v.Workspace, &v.UpdatedAt, &v.UpdatedBy)
	})
}

// AtScope returns the variables set at exactly one scope: the org when both
// ids are empty. tx needs the org setting.
func AtScope(ctx context.Context, tx pgx.Tx, projectID, workspaceID string) ([]Row, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+`
		WHERE v.project_id IS NOT DISTINCT FROM nullif($1, '')::uuid AND v.workspace_id IS NOT DISTINCT FROM nullif($2, '')::uuid
		ORDER BY v.category, v.key`, projectID, workspaceID)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// Effective returns what a run in the workspace gets: for each category and
// key, the workspace's variable over the project's over the org's. tx
// needs the org setting.
func Effective(ctx context.Context, tx pgx.Tx, workspaceID string) ([]Row, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (v.category, v.key) `+columns+`
		WHERE v.workspace_id = $1
		   OR v.project_id = (SELECT project_id FROM workspaces WHERE id = $1)
		   OR (v.project_id IS NULL AND v.workspace_id IS NULL)
		ORDER BY v.category, v.key, (v.workspace_id IS NULL), (v.project_id IS NULL)`, workspaceID)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

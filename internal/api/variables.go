package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/variables"
)

var (
	terraformKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)
	envKey       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// Names that would replace what Stackwright gives the engine: its
	// credentials, the state backend, proxies and paths.
	reservedPrefixes = []string{"TF_", "AWS_", "ARM_", "GOOGLE_", "STACKWRIGHT_"}
	reservedNames    = map[string]bool{"HOME": true, "PATH": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true}
)

// The executor finds and masks each sensitive value in output. It cannot
// mask a short one without masking common text too.
const minSensitive = 6

const maxValue = 64 << 10

type Variables struct {
	KEK seal.KEK
}

var categories = map[v1.VariableCategory]string{
	v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM: "terraform",
	v1.VariableCategory_VARIABLE_CATEGORY_ENV:       "env",
}

// scopeIDs returns the ids of a scope: none for the org, a project's, or a
// workspace's.
func scopeIDs(ctx context.Context, tx pgx.Tx, scope *v1.VariableScope) (projectID, wsID string, err error) {
	switch {
	case scope.GetWorkspace() != "":
		wsID, err = workspaceID(ctx, tx, scope.GetProject(), scope.GetWorkspace())
	case scope.GetProject() != "":
		var p projectRow
		p, err = project(ctx, tx, scope.GetProject())
		projectID = p.id
	}
	return projectID, wsID, err
}

func scopeName(scope *v1.VariableScope) string {
	switch {
	case scope.GetWorkspace() != "":
		return scope.GetProject() + "/" + scope.GetWorkspace()
	case scope.GetProject() != "":
		return scope.GetProject()
	}
	return "org"
}

func toVariable(r variables.Row) *v1.Variable {
	v := &v1.Variable{
		Key: r.Key, Sensitive: r.Sensitive, Description: r.Description, UpdatedBy: r.UpdatedBy,
		Scope: &v1.VariableScope{Project: r.Project, Workspace: r.Workspace}, UpdatedAt: timestamppb.New(r.UpdatedAt),
	}
	for c, name := range categories {
		if name == r.Category {
			v.Category = c
		}
	}
	if r.Value != nil {
		v.Value = *r.Value
	}
	return v
}

func (h Variables) ListVariables(ctx context.Context, req *connect.Request[v1.ListVariablesRequest]) (*connect.Response[v1.ListVariablesResponse], error) {
	s := authz.ScopeFrom(ctx)
	projectID, workspaceID, err := scopeIDs(ctx, s.Tx, req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	var rows []variables.Row
	switch {
	case req.Msg.GetEffective() && workspaceID == "":
		return nil, invalid("effective variables need a workspace")
	case req.Msg.GetEffective():
		rows, err = variables.Effective(ctx, s.Tx, workspaceID)
	default:
		rows, err = variables.AtScope(ctx, s.Tx, projectID, workspaceID)
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := &v1.ListVariablesResponse{}
	for _, r := range rows {
		out.Variables = append(out.Variables, toVariable(r))
	}
	return connect.NewResponse(out), nil
}

func checkVariable(category v1.VariableCategory, key, value string, sensitive bool) error {
	switch category {
	case v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM:
		if !terraformKey.MatchString(key) {
			return invalid("an input variable's name starts with a letter or _, then letters, digits, _ or -")
		}
	case v1.VariableCategory_VARIABLE_CATEGORY_ENV:
		if !envKey.MatchString(key) {
			return invalid("an environment variable's name starts with a letter or _, then letters, digits or _")
		}
		upper := strings.ToUpper(key)
		if reservedNames[upper] {
			return invalid(key + " is set by Stackwright, so it cannot be a variable")
		}
		for _, p := range reservedPrefixes {
			if strings.HasPrefix(upper, p) {
				return invalid("Stackwright sets names that start with " + p + ", so they cannot be variables")
			}
		}
	default:
		return invalid("the category is an input variable or an environment variable")
	}
	if sensitive && len(value) < minSensitive {
		return invalid(fmt.Sprintf("a sensitive value needs at least %d characters, so it can be found and masked in output", minSensitive))
	}
	if len(value) > maxValue {
		return invalid("the value is longer than 64 KiB")
	}
	return nil
}

func (h Variables) SetVariable(ctx context.Context, req *connect.Request[v1.SetVariableRequest]) (*connect.Response[v1.SetVariableResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	keep := m.GetKeepValue() && m.GetSensitive()
	if !keep {
		if err := checkVariable(m.GetCategory(), m.GetKey(), m.GetValue(), m.GetSensitive()); err != nil {
			return nil, err
		}
	}
	if len(m.GetDescription()) > 200 {
		return nil, invalid("the description is longer than 200 characters")
	}
	projectID, workspaceID, err := scopeIDs(ctx, s.Tx, m.GetScope())
	if err != nil {
		return nil, err
	}
	category := categories[m.GetCategory()]
	var value *string
	var sealed []byte
	switch {
	case keep:
		err := s.Tx.QueryRow(ctx, `SELECT value_sealed FROM variables WHERE project_id IS NOT DISTINCT FROM nullif($1, '')::uuid
			AND workspace_id IS NOT DISTINCT FROM nullif($2, '')::uuid AND category = $3 AND key = $4 AND sensitive`,
			projectID, workspaceID, category, m.GetKey()).Scan(&sealed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, invalid("there is no sensitive value to keep; enter a value")
		}
		if err != nil {
			return nil, authz.Internal(err)
		}
	case m.GetSensitive():
		if sealed, err = h.KEK.Seal(ctx, s.Tx, s.OrgID, []byte(m.GetValue())); err != nil {
			return nil, authz.Internal(err)
		}
	default:
		v := m.GetValue()
		value = &v
	}
	_, err = s.Tx.Exec(ctx, `INSERT INTO variables (id, org_id, project_id, workspace_id, category, key, sensitive, value, value_sealed, description, updated_by)
		VALUES ($1, $2, nullif($3, '')::uuid, nullif($4, '')::uuid, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (org_id, project_id, workspace_id, category, key) DO UPDATE SET sensitive = excluded.sensitive, value = excluded.value,
			value_sealed = excluded.value_sealed, description = excluded.description, updated_by = excluded.updated_by, updated_at = now()`,
		uuid.NewString(), s.OrgID, projectID, workspaceID, category, m.GetKey(), m.GetSensitive(), value, sealed, m.GetDescription(), s.UserID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	rows, err := variables.AtScope(ctx, s.Tx, projectID, workspaceID)
	if err != nil {
		return nil, authz.Internal(err)
	}
	out := &v1.SetVariableResponse{}
	for _, r := range rows {
		if r.Category == category && r.Key == m.GetKey() {
			out.Variable = toVariable(r)
		}
	}
	s.Target(fmt.Sprintf("variable %s %s:%s sensitive=%t", scopeName(m.GetScope()), category, m.GetKey(), m.GetSensitive()))
	return connect.NewResponse(out), nil
}

func (h Variables) DeleteVariable(ctx context.Context, req *connect.Request[v1.DeleteVariableRequest]) (*connect.Response[v1.DeleteVariableResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	projectID, workspaceID, err := scopeIDs(ctx, s.Tx, m.GetScope())
	if err != nil {
		return nil, err
	}
	category := categories[m.GetCategory()]
	tag, err := s.Tx.Exec(ctx, `DELETE FROM variables WHERE project_id IS NOT DISTINCT FROM nullif($1, '')::uuid
		AND workspace_id IS NOT DISTINCT FROM nullif($2, '')::uuid AND category = $3 AND key = $4`, projectID, workspaceID, category, m.GetKey())
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("variable not found"))
	}
	s.Target(fmt.Sprintf("variable %s %s:%s", scopeName(m.GetScope()), category, m.GetKey()))
	return connect.NewResponse(&v1.DeleteVariableResponse{}), nil
}

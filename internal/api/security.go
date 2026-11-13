package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/findings"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/runs"
)

const maxAcceptance = 90 * 24 * time.Hour

var (
	policyName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	checkID    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	severities = map[string]v1.Severity{"low": v1.Severity_SEVERITY_LOW, "medium": v1.Severity_SEVERITY_MEDIUM,
		"high": v1.Severity_SEVERITY_HIGH, "critical": v1.Severity_SEVERITY_CRITICAL}
)

// Security serves the security review's findings, risk acceptances and
// the org's policy (adr/0037).
type Security struct {
	stackwrightv1connect.UnimplementedSecurityServiceHandler
	Jobs         *jobs.Service
	Orchestrator *runs.Orchestrator
}

// mayDecideRisk is the security role, an admin or an owner. Roles rank by
// number, and writers and reviewers rank above security, so the list is
// explicit: those who change code do not accept its risks.
func mayDecideRisk(role v1.Role) error {
	if role == v1.Role_ROLE_SECURITY || role == v1.Role_ROLE_ADMIN || role == v1.Role_ROLE_OWNER {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("accepting a risk and changing the security policy need the security role, an admin or an owner"))
}

const findingColumns = `f.id, p.slug, w.name, f.check_id, f.severity, f.title, f.message, f.resource, f.fixed_at IS NOT NULL,
	f.first_seen_at, f.last_seen_at, a.reason, a.accepted_at, a.expires_at, coalesce(u.name, u.email)
	FROM findings f JOIN workspaces w ON w.id = f.workspace_id JOIN projects p ON p.id = w.project_id
	LEFT JOIN LATERAL (SELECT * FROM risk_acceptances x WHERE x.finding_id = f.id AND x.expires_at > now()
		ORDER BY x.expires_at DESC LIMIT 1) a ON true
	LEFT JOIN users u ON u.id = a.accepted_by`

func queryFindings(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]*v1.Finding, error) {
	rows, err := tx.Query(ctx, `SELECT `+findingColumns+` WHERE `+where, args...)
	if err != nil {
		return nil, authz.Internal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Finding, error) {
		f := &v1.Finding{}
		var severity string
		var first, last time.Time
		var reason, by *string
		var accepted, expires *time.Time
		err := r.Scan(&f.Id, &f.Project, &f.Workspace, &f.CheckId, &severity, &f.Title, &f.Message, &f.Resource, &f.Fixed,
			&first, &last, &reason, &accepted, &expires, &by)
		f.Severity, f.FirstSeenAt, f.LastSeenAt = severities[severity], timestamppb.New(first), timestamppb.New(last)
		if reason != nil {
			f.Acceptance = &v1.RiskAcceptance{Reason: *reason, AcceptedAt: timestamppb.New(*accepted), ExpiresAt: timestamppb.New(*expires)}
			if by != nil {
				f.Acceptance.AcceptedBy = *by
			}
		}
		return f, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return out, nil
}

func (Security) ListFindings(ctx context.Context, req *connect.Request[v1.ListFindingsRequest]) (*connect.Response[v1.ListFindingsResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	out, err := queryFindings(ctx, s.Tx, `w.archived_at IS NULL AND p.archived_at IS NULL AND ($1 = '' OR p.slug = $1) AND ($2 = '' OR w.name = $2)
		AND ($3 OR f.fixed_at IS NULL)
		ORDER BY f.fixed_at IS NOT NULL, CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, f.first_seen_at`,
		m.Project, m.Workspace, m.IncludeFixed)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListFindingsResponse{Findings: out}), nil
}

// ListRunFindings reads the run's own scan report, so a pull request's or
// the agent's plan shows what it found too, and marks what blocks it now.
func (h Security) ListRunFindings(ctx context.Context, req *connect.Request[v1.ListRunFindingsRequest]) (*connect.Response[v1.ListRunFindingsResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	id, err := findRun(ctx, s.Tx, m.Project, m.Workspace, m.Number)
	if err != nil {
		return nil, err
	}
	var planJob *string
	var workspaceID, status string
	if err := s.Tx.QueryRow(ctx, `SELECT plan_job_id, workspace_id, status FROM runs WHERE id = $1`, id).Scan(&planJob, &workspaceID, &status); err != nil {
		return nil, authz.Internal(err)
	}
	if planJob == nil {
		return connect.NewResponse(&v1.ListRunFindingsResponse{}), nil
	}
	raw, err := h.Jobs.ReadArtifact(ctx, s.Tx, *planJob, "scan.json")
	if errors.Is(err, jobs.ErrNoArtifact) {
		return connect.NewResponse(&v1.ListRunFindingsResponse{}), nil
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	results, err := findings.Parse(raw)
	if err != nil {
		return nil, authz.Internal(err)
	}
	var blocking []string
	if status == "blocked" || status == "needs_approval" {
		plan, err := h.Jobs.ReadArtifact(ctx, s.Tx, *planJob, "plan.json")
		if err != nil {
			return nil, authz.Internal(err)
		}
		changed, err := findings.Changed(plan)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if blocking, err = findings.Blocking(ctx, s.Tx, workspaceID, changed); err != nil {
			return nil, authz.Internal(err)
		}
	}
	known, err := queryFindings(ctx, s.Tx, `f.workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*v1.Finding, 0, len(results))
	for _, r := range results {
		f := &v1.Finding{Project: m.Project, Workspace: m.Workspace, CheckId: r.CheckID, Severity: severities[r.Severity], Title: r.Title,
			Message: r.Message, Resource: r.Resource}
		if i := slices.IndexFunc(known, func(k *v1.Finding) bool { return k.CheckId == r.CheckID && k.Resource == r.Resource }); i >= 0 {
			f.Id, f.Acceptance, f.FirstSeenAt = known[i].Id, known[i].Acceptance, known[i].FirstSeenAt
			f.Blocking = slices.Contains(blocking, f.Id)
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b *v1.Finding) int { return int(b.Severity) - int(a.Severity) })
	return connect.NewResponse(&v1.ListRunFindingsResponse{Findings: out}), nil
}

func (h Security) AcceptRisk(ctx context.Context, req *connect.Request[v1.AcceptRiskRequest]) (*connect.Response[v1.AcceptRiskResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if err := mayDecideRisk(s.Role); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(m.Reason)
	if reason == "" || len(reason) > 2000 {
		return nil, invalid("say why the risk is acceptable, in 1 to 2,000 characters")
	}
	if m.ExpiresAt == nil {
		return nil, invalid("an accepted risk needs an expiry")
	}
	expires := m.ExpiresAt.AsTime()
	if until := time.Until(expires); until <= 0 || until > maxAcceptance {
		return nil, invalid("an accepted risk expires within 90 days")
	}
	if _, err := uuid.Parse(m.Id); err != nil {
		return nil, errNotFound
	}
	var workspaceID string
	err := s.Tx.QueryRow(ctx, `SELECT workspace_id FROM findings WHERE id = $1 AND fixed_at IS NULL`, m.Id).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO risk_acceptances (id, org_id, finding_id, accepted_by, reason, expires_at)
		VALUES ($1, app_org_id(), $2, $3, $4, $5)`, uuid.NewString(), m.Id, s.UserID, reason, expires); err != nil {
		return nil, authz.Internal(err)
	}
	if err := h.Orchestrator.Unblock(ctx, s.Tx, workspaceID); err != nil {
		return nil, authz.Internal(err)
	}
	fs, err := queryFindings(ctx, s.Tx, `f.id = $1`, m.Id)
	if err != nil {
		return nil, err
	}
	s.Target(fmt.Sprintf("finding %s %s on %s, until %s", fs[0].CheckId, m.Id, fs[0].Resource, expires.Format(time.RFC3339)))
	return connect.NewResponse(&v1.AcceptRiskResponse{Finding: fs[0]}), nil
}

func (Security) GetSecurityPolicy(ctx context.Context, _ *connect.Request[v1.GetSecurityPolicyRequest]) (*connect.Response[v1.GetSecurityPolicyResponse], error) {
	s := authz.ScopeFrom(ctx)
	rows, err := s.Tx.Query(ctx, `SELECT p.name, p.rego, coalesce(u.name, u.email, ''), p.updated_at FROM policy_sets p
		LEFT JOIN users u ON u.id = p.updated_by ORDER BY p.name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	sets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.PolicySet, error) {
		p := &v1.PolicySet{}
		var at time.Time
		err := r.Scan(&p.Name, &p.Rego, &p.UpdatedBy, &at)
		p.UpdatedAt = timestamppb.New(at)
		return p, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	rows, err = s.Tx.Query(ctx, `SELECT check_id FROM skipped_checks ORDER BY check_id`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	skipped, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.GetSecurityPolicyResponse{PolicySets: sets, SkippedChecks: skipped}), nil
}

// SetPolicySet stores Rego in Trivy's check format. Trivy loads only the
// user namespace from it, so a set outside that namespace would never run.
func (Security) SetPolicySet(ctx context.Context, req *connect.Request[v1.SetPolicySetRequest]) (*connect.Response[v1.SetPolicySetResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	if err := mayDecideRisk(s.Role); err != nil {
		return nil, err
	}
	if !policyName.MatchString(m.Name) {
		return nil, invalid("a policy set's name is 1 to 40 lowercase letters, digits and hyphens")
	}
	if !regexp.MustCompile(`(?m)^package user\.`).MatchString(m.Rego) || len(m.Rego) > 256<<10 {
		return nil, invalid("a policy set is Rego in Trivy's check format, in a package under user., such as package user.acme.ACME0001")
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO policy_sets (org_id, name, rego, updated_by) VALUES (app_org_id(), $1, $2, $3)
		ON CONFLICT (org_id, name) DO UPDATE SET rego = $2, updated_by = $3, updated_at = now()`, m.Name, m.Rego, s.UserID); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("policy set " + m.Name)
	return connect.NewResponse(&v1.SetPolicySetResponse{PolicySet: &v1.PolicySet{Name: m.Name, Rego: m.Rego}}), nil
}

func (Security) DeletePolicySet(ctx context.Context, req *connect.Request[v1.DeletePolicySetRequest]) (*connect.Response[v1.DeletePolicySetResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := mayDecideRisk(s.Role); err != nil {
		return nil, err
	}
	tag, err := s.Tx.Exec(ctx, `DELETE FROM policy_sets WHERE name = $1`, req.Msg.Name)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	s.Target("policy set " + req.Msg.Name)
	return connect.NewResponse(&v1.DeletePolicySetResponse{}), nil
}

func (Security) SetSkippedChecks(ctx context.Context, req *connect.Request[v1.SetSkippedChecksRequest]) (*connect.Response[v1.SetSkippedChecksResponse], error) {
	s := authz.ScopeFrom(ctx)
	if err := mayDecideRisk(s.Role); err != nil {
		return nil, err
	}
	if len(req.Msg.SkippedChecks) > 500 {
		return nil, invalid("skip at most 500 checks")
	}
	for _, id := range req.Msg.SkippedChecks {
		if !checkID.MatchString(id) {
			return nil, invalid(fmt.Sprintf("%q is not a check id, such as AWS-0089", id))
		}
	}
	if _, err := s.Tx.Exec(ctx, `DELETE FROM skipped_checks`); err != nil {
		return nil, authz.Internal(err)
	}
	for _, id := range req.Msg.SkippedChecks {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO skipped_checks (org_id, check_id) VALUES (app_org_id(), $1) ON CONFLICT DO NOTHING`, id); err != nil {
			return nil, authz.Internal(err)
		}
	}
	s.Target(fmt.Sprintf("skipped checks: %d", len(req.Msg.SkippedChecks)))
	return connect.NewResponse(&v1.SetSkippedChecksResponse{}), nil
}

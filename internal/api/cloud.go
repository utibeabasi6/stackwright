package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/oidc"
)

var (
	// The aws partition only: executors call the global STS endpoint, which
	// does not serve China or GovCloud. The groups are the account, the
	// role's path and its name.
	roleARN   = regexp.MustCompile(`^arn:aws:iam::([0-9]{12}):role/((?:[\w+=,.@-]+/)*)([\w+=,.@-]{1,64})$`)
	awsRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

type CloudAccounts struct {
	Issuer *oidc.Issuer
}

// issuer is empty when tokens cannot be minted, because AWS needs https.
func (h CloudAccounts) issuer() string {
	if strings.HasPrefix(h.Issuer.URL, "https://") {
		return h.Issuer.URL
	}
	return ""
}

func (h CloudAccounts) ListCloudAccounts(ctx context.Context, _ *connect.Request[v1.ListCloudAccountsRequest]) (*connect.Response[v1.ListCloudAccountsResponse], error) {
	s := authz.ScopeFrom(ctx)
	accounts, err := loadAccounts(ctx, s.Tx, "")
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListCloudAccountsResponse{Accounts: accounts, Issuer: h.issuer()}), nil
}

func (h CloudAccounts) CreateCloudAccount(ctx context.Context, req *connect.Request[v1.CreateCloudAccountRequest]) (*connect.Response[v1.CreateCloudAccountResponse], error) {
	s := authz.ScopeFrom(ctx)
	name, aws := req.Msg.GetName(), req.Msg.GetAws()
	if !slugPattern.MatchString(name) {
		return nil, invalid("the name needs 1 to 40 lowercase letters, digits or hyphens")
	}
	plan, apply := roleARN.FindStringSubmatch(aws.GetPlanRoleArn()), roleARN.FindStringSubmatch(aws.GetApplyRoleArn())
	switch {
	case plan == nil || apply == nil:
		return nil, invalid("a role ARN looks like arn:aws:iam::123456789012:role/name, in the aws partition")
	case plan[1] != apply[1]:
		return nil, invalid("the plan role and the apply role must be in the same AWS account")
	case aws.GetPlanRoleArn() == aws.GetApplyRoleArn():
		return nil, invalid("the plan role must differ from the apply role, so that a plan cannot write")
	case !awsRegion.MatchString(aws.GetRegion()):
		return nil, invalid("a region looks like eu-west-1")
	}
	_, err := s.Tx.Exec(ctx, `INSERT INTO cloud_accounts (id, org_id, name, provider, aws_plan_role_arn, aws_apply_role_arn, aws_region)
		VALUES ($1, $2, $3, 'aws', $4, $5, $6)`, uuid.NewString(), s.OrgID, name, aws.GetPlanRoleArn(), aws.GetApplyRoleArn(), aws.GetRegion())
	if uniqueViolation(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("a cloud account with that name exists"))
	}
	if err != nil {
		return nil, authz.Internal(err)
	}
	accounts, err := loadAccounts(ctx, s.Tx, name)
	if err != nil {
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("cloud account %s plan=%s apply=%s", name, aws.GetPlanRoleArn(), aws.GetApplyRoleArn()))
	return connect.NewResponse(&v1.CreateCloudAccountResponse{Account: accounts[0]}), nil
}

func (h CloudAccounts) DeleteCloudAccount(ctx context.Context, req *connect.Request[v1.DeleteCloudAccountRequest]) (*connect.Response[v1.DeleteCloudAccountResponse], error) {
	s := authz.ScopeFrom(ctx)
	a, err := oneAccount(ctx, s.Tx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if len(a.GetWorkspaces()) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s is used by %s; choose another account for them first", a.GetName(), strings.Join(a.GetWorkspaces(), ", ")))
	}
	if _, err := s.Tx.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, a.GetId()); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("cloud account " + a.GetName())
	return connect.NewResponse(&v1.DeleteCloudAccountResponse{}), nil
}

func (h CloudAccounts) SetWorkspaceCloudAccount(ctx context.Context, req *connect.Request[v1.SetWorkspaceCloudAccountRequest]) (*connect.Response[v1.SetWorkspaceCloudAccountResponse], error) {
	s := authz.ScopeFrom(ctx)
	ws, err := workspaceID(ctx, s.Tx, req.Msg.GetProject(), req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	var accountID *string
	if req.Msg.GetAccount() != "" {
		a, err := oneAccount(ctx, s.Tx, req.Msg.GetAccount())
		if err != nil {
			return nil, err
		}
		id := a.GetId()
		accountID = &id
	}
	if _, err := s.Tx.Exec(ctx, `UPDATE workspaces SET cloud_account_id = $2 WHERE id = $1`, ws, accountID); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target(fmt.Sprintf("workspace:%s/%s cloud_account=%s", req.Msg.GetProject(), req.Msg.GetWorkspace(), req.Msg.GetAccount()))
	return connect.NewResponse(&v1.SetWorkspaceCloudAccountResponse{}), nil
}

func (h CloudAccounts) GetAwsSetup(ctx context.Context, req *connect.Request[v1.GetAwsSetupRequest]) (*connect.Response[v1.GetAwsSetupResponse], error) {
	s := authz.ScopeFrom(ctx)
	issuer := h.issuer()
	if issuer == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"AWS trusts only an https issuer, and this instance's is %s; set STACKWRIGHT_OIDC_ISSUER", h.Issuer.URL))
	}
	a, err := oneAccount(ctx, s.Tx, req.Msg.GetAccount())
	if err != nil {
		return nil, err
	}
	rows, err := s.Tx.Query(ctx, `SELECT id FROM workspaces WHERE cloud_account_id = $1 AND archived_at IS NULL ORDER BY id`, a.GetId())
	if err != nil {
		return nil, authz.Internal(err)
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, authz.Internal(err)
	}
	if len(workspaces) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no workspace uses this account yet; choose it on a workspace first"))
	}
	plan, apply := roleARN.FindStringSubmatch(a.GetAws().GetPlanRoleArn()), roleARN.FindStringSubmatch(a.GetAws().GetApplyRoleArn())
	host := strings.TrimPrefix(issuer, "https://")
	provider := fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", plan[1], host)
	// Each role trusts tokens of these workspaces for its own phase only, so
	// AWS refuses another workspace's token, and a plan token on the apply
	// role.
	trust := func(phase string) map[string]any {
		var subs []string
		for _, w := range workspaces {
			subs = append(subs, oidc.Subject(s.OrgID, w, phase))
		}
		return map[string]any{
			"Version": "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect":    "Allow",
				"Principal": map[string]any{"Federated": provider},
				"Action":    "sts:AssumeRoleWithWebIdentity",
				"Condition": map[string]any{"StringEquals": map[string]any{host + ":aud": oidc.AWSAudience, host + ":sub": subs}},
			}},
		}
	}
	pretty := func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
	inline := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	// CloudFormation takes a role's path apart from its name.
	role := func(m []string) string {
		if m[2] == "" {
			return "RoleName: " + m[3]
		}
		return "RoleName: " + m[3] + "\n      Path: /" + m[2]
	}
	// YAML holds JSON as it is, so the trust policies go in unchanged.
	template := fmt.Sprintf(`AWSTemplateFormatVersion: "2010-09-09"
Description: Stackwright runs. Plans assume %[1]s and applies assume %[2]s, with no stored keys.
Parameters:
  CreateOIDCProvider:
    Type: String
    AllowedValues: ["true", "false"]
    Default: "true"
    Description: Set to false if this account already trusts %[3]s.
  PlanPolicyArn:
    Type: String
    Default: arn:aws:iam::aws:policy/ReadOnlyAccess
    Description: What plans may do. Keep it read-only.
  ApplyPolicyArn:
    Type: String
    Default: arn:aws:iam::aws:policy/AdministratorAccess
    Description: What applies may do. Narrow it if you can.
Conditions:
  MakeProvider: !Equals [!Ref CreateOIDCProvider, "true"]
Resources:
  StackwrightOIDCProvider:
    Type: AWS::IAM::OIDCProvider
    Condition: MakeProvider
    Properties:
      Url: %[3]s
      ClientIdList: [%[4]s]
  PlanRole:
    Type: AWS::IAM::Role
    Properties:
      %[5]s
      MaxSessionDuration: 3600
      ManagedPolicyArns: [!Ref PlanPolicyArn]
      AssumeRolePolicyDocument: %[6]s
  ApplyRole:
    Type: AWS::IAM::Role
    Properties:
      %[7]s
      MaxSessionDuration: 3600
      ManagedPolicyArns: [!Ref ApplyPolicyArn]
      AssumeRolePolicyDocument: %[8]s
`, plan[2]+plan[3], apply[2]+apply[3], issuer, oidc.AWSAudience, role(plan), inline(trust("plan")), role(apply), inline(trust("apply")))
	return connect.NewResponse(&v1.GetAwsSetupResponse{
		Issuer: issuer, PlanRoleTrustPolicy: pretty(trust("plan")), ApplyRoleTrustPolicy: pretty(trust("apply")), CloudformationTemplate: template,
	}), nil
}

func oneAccount(ctx context.Context, tx pgx.Tx, name string) (*v1.CloudAccount, error) {
	if name == "" {
		return nil, invalid("name the cloud account")
	}
	accounts, err := loadAccounts(ctx, tx, name)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if len(accounts) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("cloud account not found"))
	}
	return accounts[0], nil
}

// loadAccounts returns the org's accounts, or the one named. tx needs the
// org setting.
func loadAccounts(ctx context.Context, tx pgx.Tx, name string) ([]*v1.CloudAccount, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name, a.aws_plan_role_arn, a.aws_apply_role_arn, a.aws_region, a.created_at,
		       coalesce(array_agg(p.slug || '/' || w.name ORDER BY p.slug, w.name) FILTER (WHERE w.id IS NOT NULL), '{}')
		FROM cloud_accounts a
		LEFT JOIN workspaces w ON w.cloud_account_id = a.id AND w.archived_at IS NULL
		LEFT JOIN projects p ON p.id = w.project_id
		WHERE $1 = '' OR a.name = $1
		GROUP BY a.id ORDER BY a.name`, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.CloudAccount, error) {
		a := &v1.CloudAccount{Aws: &v1.AwsAccount{}}
		var created time.Time
		err := r.Scan(&a.Id, &a.Name, &a.Aws.PlanRoleArn, &a.Aws.ApplyRoleArn, &a.Aws.Region, &created, &a.Workspaces)
		a.CreatedAt = timestamppb.New(created)
		return a, err
	})
}

package authz

import (
	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
)

type Access int

const (
	Public Access = iota + 1
	SignedIn
	InstanceAdmin
	InOrg
	// JobTicket takes a job's one-time ticket as the bearer token, and
	// spends it.
	JobTicket
	// Job takes a job token as the bearer token. Each call is a heartbeat.
	Job
	// Builder takes a build server's client certificate, which the builder
	// port's TLS handshake verified.
	Builder
)

type Rule struct {
	Access Access
	Min    v1.Role
}

func org(min v1.Role) Rule { return Rule{Access: InOrg, Min: min} }

// Policy lists every RPC. The interceptor refuses one that is not here.
var Policy = map[string]Rule{
	stackwrightv1connect.MetaServiceGetVersionProcedure: {Access: Public},

	stackwrightv1connect.AuthServiceGetSetupProcedure:            {Access: Public},
	stackwrightv1connect.AuthServiceCompleteSetupProcedure:       {Access: Public},
	stackwrightv1connect.AuthServiceSignInProcedure:              {Access: Public},
	stackwrightv1connect.AuthServiceListSignInProvidersProcedure: {Access: Public},
	stackwrightv1connect.AuthServiceGetInviteProcedure:           {Access: Public},
	stackwrightv1connect.AuthServiceAcceptInviteProcedure:        {Access: Public},
	stackwrightv1connect.AuthServiceSignOutProcedure:             {Access: SignedIn},
	stackwrightv1connect.AuthServiceGetMeProcedure:               {Access: SignedIn},

	stackwrightv1connect.InstanceServiceListSignInProviderSettingsProcedure: {Access: InstanceAdmin},
	stackwrightv1connect.InstanceServiceCreateSignInProviderProcedure:       {Access: InstanceAdmin},
	stackwrightv1connect.InstanceServiceDeleteSignInProviderProcedure:       {Access: InstanceAdmin},
	stackwrightv1connect.OrgServiceCreateOrgProcedure:                       {Access: InstanceAdmin},

	stackwrightv1connect.OrgServiceListMembersProcedure:     org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.OrgServiceSetMemberRoleProcedure:   org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.OrgServiceRemoveMemberProcedure:    org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.OrgServiceCreateInviteProcedure:    org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.OrgServiceListInvitesProcedure:     org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.OrgServiceRevokeInviteProcedure:    org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.OrgServiceListAuditEventsProcedure: org(v1.Role_ROLE_ADMIN),

	stackwrightv1connect.ProjectServiceListProjectsProcedure:        org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.ProjectServiceGetWorkspaceProcedure:        org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.ProjectServiceCreateProjectProcedure:       org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceCreateSampleProjectProcedure: org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceUpdateProjectProcedure:       org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceArchiveProjectProcedure:      org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceCreateWorkspaceProcedure:     org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceArchiveWorkspaceProcedure:    org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.ProjectServiceUpdateWorkspaceProcedure:     org(v1.Role_ROLE_ADMIN),

	stackwrightv1connect.RunServiceCreateRunProcedure:   org(v1.Role_ROLE_WRITER),
	stackwrightv1connect.RunServiceListRunsProcedure:    org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.RunServiceGetRunProcedure:      org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.RunServiceWatchRunProcedure:    org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.RunServiceApproveRunProcedure:  org(v1.Role_ROLE_REVIEWER),
	stackwrightv1connect.RunServiceRejectRunProcedure:   org(v1.Role_ROLE_REVIEWER),
	stackwrightv1connect.RunServiceCancelRunProcedure:   org(v1.Role_ROLE_WRITER),
	stackwrightv1connect.RunServiceListWaitingProcedure: org(v1.Role_ROLE_VIEWER),

	stackwrightv1connect.StateServiceGetStateProcedure:          org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.StateServiceListStateVersionsProcedure: org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.StateServiceGetStateVersionProcedure:   org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.StateServiceRollbackStateProcedure:     org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.StateServiceForceUnlockStateProcedure:  org(v1.Role_ROLE_ADMIN),

	stackwrightv1connect.TokenServiceListApiTokensProcedure:  org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.TokenServiceCreateApiTokenProcedure: org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.TokenServiceRevokeApiTokenProcedure: org(v1.Role_ROLE_VIEWER),

	stackwrightv1connect.ExecutorServiceClaimJobProcedure:     {Access: JobTicket},
	stackwrightv1connect.ExecutorServiceReportEventsProcedure: {Access: Job},
	stackwrightv1connect.ExecutorServiceFinishJobProcedure:    {Access: Job},

	stackwrightv1connect.BuilderServiceEnrolProcedure:   {Access: Public},
	stackwrightv1connect.BuilderServiceConnectProcedure: {Access: Builder},

	stackwrightv1connect.BuildServerServiceListBuildServersProcedure:  org(v1.Role_ROLE_VIEWER),
	stackwrightv1connect.BuildServerServiceUpdateBuildServerProcedure: org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.BuildServerServiceRemoveBuildServerProcedure: org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.BuildServerServiceGetSSHKeyProcedure:         org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.BuildServerServiceScanHostKeyProcedure:       org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.BuildServerServiceEnrolOverSSHProcedure:      org(v1.Role_ROLE_ADMIN),
	stackwrightv1connect.BuildServerServiceCreateJoinCommandProcedure: org(v1.Role_ROLE_ADMIN),
}

var rank = map[v1.Role]int{
	v1.Role_ROLE_VIEWER:   1,
	v1.Role_ROLE_SECURITY: 2,
	v1.Role_ROLE_REVIEWER: 3,
	v1.Role_ROLE_WRITER:   4,
	v1.Role_ROLE_ADMIN:    5,
	v1.Role_ROLE_OWNER:    6,
}

func AtLeast(r, min v1.Role) bool {
	return rank[r] > 0 && rank[r] >= rank[min]
}

func ValidRole(r v1.Role) bool {
	return rank[r] > 0
}

import { ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AgentService } from "./gen/stackwright/v1/agent_pb";
import { AuthService } from "./gen/stackwright/v1/auth_pb";
import { BuildServerService } from "./gen/stackwright/v1/build_servers_pb";
import { CloudAccountService } from "./gen/stackwright/v1/cloud_pb";
import { GitHubService } from "./gen/stackwright/v1/github_pb";
import { GraphService } from "./gen/stackwright/v1/graph_pb";
import { ImportService } from "./gen/stackwright/v1/imports_pb";
import { VariableService } from "./gen/stackwright/v1/variables_pb";
import { InstanceService } from "./gen/stackwright/v1/instance_pb";
import { MetaService } from "./gen/stackwright/v1/meta_pb";
import { OrgService } from "./gen/stackwright/v1/orgs_pb";
import { ProjectService } from "./gen/stackwright/v1/projects_pb";
import { RunService } from "./gen/stackwright/v1/runs_pb";
import { SecurityService } from "./gen/stackwright/v1/security_pb";
import { StateService, TokenService } from "./gen/stackwright/v1/state_pb";

const csrf: Interceptor = (next) => (req) => {
  const token = document.cookie.match(/(?:^|; )sw_csrf=([^;]*)/)?.[1];
  if (token) req.header.set("X-Stackwright-Csrf", decodeURIComponent(token));
  return next(req);
};

const transport = createConnectTransport({ baseUrl: window.location.origin, interceptors: [csrf] });

export const meta = createClient(MetaService, transport);
export const authApi = createClient(AuthService, transport);
export const orgApi = createClient(OrgService, transport);
export const instanceApi = createClient(InstanceService, transport);
export const projectApi = createClient(ProjectService, transport);
export const stateApi = createClient(StateService, transport);
export const tokenApi = createClient(TokenService, transport);
export const runApi = createClient(RunService, transport);
export const buildServerApi = createClient(BuildServerService, transport);
export const variableApi = createClient(VariableService, transport);
export const cloudApi = createClient(CloudAccountService, transport);
export const importApi = createClient(ImportService, transport);
export const githubApi = createClient(GitHubService, transport);
export const graphApi = createClient(GraphService, transport);
export const agentApi = createClient(AgentService, transport);
export const securityApi = createClient(SecurityService, transport);

export function message(err: unknown): string {
  return err instanceof ConnectError ? err.rawMessage : "Something went wrong. Try again.";
}

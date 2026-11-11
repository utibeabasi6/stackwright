import { ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AuthService } from "./gen/stackwright/v1/auth_pb";
import { InstanceService } from "./gen/stackwright/v1/instance_pb";
import { MetaService } from "./gen/stackwright/v1/meta_pb";
import { OrgService } from "./gen/stackwright/v1/orgs_pb";

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

export function message(err: unknown): string {
  return err instanceof ConnectError ? err.rawMessage : "Something went wrong. Try again.";
}

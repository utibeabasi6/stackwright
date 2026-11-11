import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { MetaService } from "./gen/stackwright/meta/v1/meta_pb";

export const transport = createConnectTransport({ baseUrl: window.location.origin });
export const meta = createClient(MetaService, transport);

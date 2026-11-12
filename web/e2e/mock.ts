import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";

export type Graph = {
  nodes: { id: string; module?: string; action: string; instances?: number; counts?: unknown[] }[];
  edges?: unknown[];
  groups?: unknown[];
};

export const testdata = <T>(name: string, file: string): T =>
  JSON.parse(readFileSync(join(process.cwd(), "..", "internal", "graph", "testdata", name, file), "utf8"));

export const golden = (name: string) => testdata<Graph>(name, "graph.json");

// A server stream's messages, ended by an error when error is set.
export type Stream = { messages: unknown[]; error?: string };

// envelope frames a stream as the Connect protocol does: a flag byte, a
// 4-byte length, then JSON; the last frame (flag 2) ends the stream.
// https://connectrpc.com/docs/protocol/#streaming-response
function envelope(s: Stream) {
  const frame = (flag: number, body: unknown) => {
    const json = Buffer.from(JSON.stringify(body));
    const head = Buffer.alloc(5);
    head.writeUInt8(flag, 0);
    head.writeUInt32BE(json.length, 1);
    return Buffer.concat([head, json]);
  };
  const end = s.error ? { error: { code: "unavailable", message: s.error } } : {};
  return Buffer.concat([...s.messages.map((m) => frame(0, m)), frame(2, end)]);
}

type Handler = (request: Record<string, unknown>) => unknown | Promise<unknown>;

// mockApi answers the app's API calls in the browser, signed in as Kim with
// role in org "a". A procedure with no handler gets an empty message.
export async function mockApi(page: Page, role: string, handlers: Record<string, Handler>) {
  const all: Record<string, Handler> = {
    GetMe: () => ({ user: { id: "u1", email: "kim@a.test", name: "Kim" }, orgs: [{ slug: "a", name: "Acme", role }] }),
    ListProjects: () => ({ projects: [{ slug: "lab", workspaces: [{ name: "default" }] }] }),
    ...handlers,
  };
  await page.route("**/stackwright.v1.*/*", async (route) => {
    const procedure = new URL(route.request().url()).pathname.split("/").pop() ?? "";
    const streaming = (route.request().headers()["content-type"] ?? "").includes("connect+json");
    const raw = route.request().postDataBuffer() ?? Buffer.alloc(0);
    const request = JSON.parse((streaming ? raw.subarray(5) : raw).toString() || "{}");
    const out = (await all[procedure]?.(request)) ?? {};
    if (streaming) {
      await route.fulfill({ contentType: "application/connect+json", body: envelope(out as Stream) });
    } else {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(out) });
    }
  });
}

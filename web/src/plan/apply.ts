import { PlanAction, type PlanGraph } from "../gen/stackwright/v1/graph_pb";
import type { RunEvent } from "../gen/stackwright/v1/runs_pb";

export type Status = "waiting" | "running" | "done" | "failed";

export type Diagnostic = { summary: string; detail: string; file: string; line: number };

type Progress = { started: number; completed: number; failed: number };

// ApplyState is what one run's apply events add up to. step returns a new
// state; live and replay both build it with step, one event at a time.
export type ApplyState = {
  // The last event folded in: an event at or below it is skipped.
  seq: bigint;
  started: boolean;
  progress: Map<string, Progress>;
  diagnostics: Map<string, Diagnostic[]>;
};

export const emptyState = (): ApplyState => ({ seq: 0n, started: false, progress: new Map(), diagnostics: new Map() });

// stripKeys removes instance keys: `module.a["x.y"].b[0]` becomes `module.a.b`.
// internal/graph has the same function; both test against
// internal/graph/testdata/keys.json.
export function stripKeys(addr: string) {
  let out = "";
  let depth = 0;
  let quoted = false;
  let escaped = false;
  for (const c of addr) {
    if (escaped) escaped = false;
    else if (quoted && c === "\\") escaped = true;
    else if (c === '"' && depth > 0) quoted = !quoted;
    else if (quoted) continue;
    else if (c === "[") depth++;
    else if (c === "]") depth--;
    else if (depth === 0) out += c;
  }
  return out;
}

const parse = (payload: string): Record<string, unknown> => {
  try {
    return JSON.parse(payload);
  } catch {
    return {};
  }
};

export function step(s: ApplyState, e: RunEvent): ApplyState {
  if (e.seq <= s.seq) return s;
  const next = { ...s, seq: e.seq };
  const p = parse(e.payload);
  if (e.type === "run.dispatched" && p.operation === "apply") {
    return { seq: e.seq, started: true, progress: new Map(), diagnostics: new Map() };
  }
  if (!s.started || !e.resourceAddr) return next;
  const id = stripKeys(e.resourceAddr);
  const field = { "engine.apply_start": "started", "engine.apply_complete": "completed", "engine.apply_errored": "failed" }[e.type] as
    | keyof Progress
    | undefined;
  if (field) {
    const have = s.progress.get(id) ?? { started: 0, completed: 0, failed: 0 };
    next.progress = new Map(s.progress).set(id, { ...have, [field]: have[field] + 1 });
  }
  const d = (p.diagnostic ?? {}) as { severity?: string; summary?: string; detail?: string; range?: { filename?: string; start?: { line?: number } } };
  if (e.type === "engine.diagnostic" && d.severity === "error") {
    const diag = { summary: d.summary ?? "", detail: d.detail ?? "", file: d.range?.filename ?? "", line: d.range?.start?.line ?? 0 };
    next.diagnostics = new Map(s.diagnostics).set(id, [...(s.diagnostics.get(id) ?? []), diag]);
  }
  return next;
}

export type NodeStatus = { status: Status; completed: number; expected: number };

// statuses gives each node that changes its status. A replace runs a create
// and a delete, so it counts twice towards done.
export function statuses(g: PlanGraph, s: ApplyState): Map<string, NodeStatus> {
  const out = new Map<string, NodeStatus>();
  if (!s.started) return out;
  for (const n of g.nodes) {
    const expected = n.counts.reduce((sum, c) => {
      if (c.action === PlanAction.NO_OP || c.action === PlanAction.UNSPECIFIED) return sum;
      const replace = c.action === PlanAction.REPLACE || c.action === PlanAction.REPLACE_CREATE_FIRST;
      return sum + c.count * (replace ? 2 : 1);
    }, 0);
    if (expected === 0) continue;
    const p = s.progress.get(n.id) ?? { started: 0, completed: 0, failed: 0 };
    let status: Status = "waiting";
    if (p.failed > 0 || s.diagnostics.has(n.id)) status = "failed";
    else if (p.completed >= expected) status = "done";
    else if (p.started > 0) status = "running";
    out.set(n.id, { status, completed: p.completed, expected });
  }
  return out;
}

const maxGap = 2000;

// timeline gives each event a time in ms from the first, with every gap cut
// to at most 2 s.
export function timeline(events: RunEvent[]): number[] {
  const out: number[] = [];
  let t = 0;
  let last: number | undefined;
  for (const e of events) {
    const at = e.at ? Number(e.at.seconds) * 1000 + e.at.nanos / 1e6 : (last ?? 0);
    if (last !== undefined) t += Math.min(Math.max(at - last, 0), maxGap);
    last = at;
    out.push(t);
  }
  return out;
}

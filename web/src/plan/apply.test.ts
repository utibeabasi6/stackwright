import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fromJson } from "@bufbuild/protobuf";
import { PlanGraphSchema } from "../gen/stackwright/v1/graph_pb";
import { RunEventSchema, type RunEvent } from "../gen/stackwright/v1/runs_pb";
import { emptyState, statuses, step, stripKeys, timeline, type ApplyState } from "./apply";

const testdata = (...parts: string[]) => readFileSync(join(process.cwd(), "..", "internal", "graph", "testdata", ...parts), "utf8");
const graph = (name: string) => fromJson(PlanGraphSchema, JSON.parse(testdata(name, "graph.json")));
const events = (name: string): RunEvent[] => JSON.parse(testdata(name, "apply-events.json")).map((e: unknown) => fromJson(RunEventSchema, e as never));
const fold = (es: RunEvent[], from: ApplyState = emptyState()) => es.reduce(step, from);
const plain = (m: Map<string, { status: string }>) => Object.fromEntries([...m].map(([id, s]) => [id, s.status]));

test("stripKeys agrees with the Go cases", () => {
  for (const c of JSON.parse(testdata("keys.json")) as { in: string; want: string }[]) expect(stripKeys(c.in)).toBe(c.want);
});

test("each node reaches done at its last completion, in event order", () => {
  const g = graph("actions");
  const es = events("actions");
  const doneAt = new Map<string, number>();
  let s = emptyState();
  es.forEach((e, i) => {
    s = step(s, e);
    for (const [id, st] of statuses(g, s)) if (st.status === "done" && !doneAt.has(id)) doneAt.set(id, i);
  });
  const lastCompletion = new Map<string, number>();
  es.forEach((e, i) => e.type === "engine.apply_complete" && lastCompletion.set(stripKeys(e.resourceAddr), i));
  expect(doneAt).toEqual(lastCompletion);
  expect(Object.values(plain(statuses(g, s)))).toEqual(Array(doneAt.size).fill("done"));
  expect(statuses(g, s).get("terraform_data.recreate_first")).toMatchObject({ completed: 2, expected: 2 });
});

test("a failed node holds its diagnostic, and what never started stays waiting", () => {
  const g = graph("failing");
  const s = fold(events("failing"));
  expect(plain(statuses(g, s))).toEqual({ "terraform_data.first": "done", "local_file.blocked": "failed", "terraform_data.after": "waiting" });
  expect(s.diagnostics.get("local_file.blocked")?.[0]).toMatchObject({ summary: "Create local file error", file: "main.tf", line: 10 });
});

test("a stream that drops and resumes, or sends an event twice, ends where one stream does", () => {
  const g = graph("actions");
  const es = events("actions");
  const whole = fold(es);
  let s = emptyState();
  for (let i = 0; i < es.length; i += 7) s = fold(es.slice(Math.max(0, i - 3), i + 7), s);
  expect(plain(statuses(g, s))).toEqual(plain(statuses(g, whole)));
  expect(s.seq).toBe(whole.seq);
});

test("a replay matches the live run event for event", () => {
  const g = graph("actions");
  const es = events("actions");
  let live = emptyState();
  es.forEach((e, i) => {
    live = step(live, e);
    expect(plain(statuses(g, fold(es.slice(0, i + 1))))).toEqual(plain(statuses(g, live)));
  });
});

test("the replay timeline cuts gaps to 2 s", () => {
  const es = ["00:00:00", "00:00:01", "00:01:01", "00:01:02"].map((t, i) =>
    fromJson(RunEventSchema, { seq: String(i + 1), type: "x", at: `2026-01-01T${t}Z` }),
  );
  expect(timeline(es)).toEqual([0, 1000, 3000, 4000]);
});

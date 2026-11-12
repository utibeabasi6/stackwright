import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fromJson } from "@bufbuild/protobuf";
import { PlanAction, PlanGraphSchema } from "../gen/stackwright/v1/graph_pb";
import { boxes, layout, nodeHeight, nodeWidth, place, visible } from "./layout";

const golden = (name: string) =>
  fromJson(PlanGraphSchema, JSON.parse(readFileSync(join(process.cwd(), "..", "internal", "graph", "testdata", name, "graph.json"), "utf8")));

test("closed modules add up what they hold, and edges move to them", () => {
  const g = golden("modules");
  const v = visible(g, true, new Set(), false);
  const net = v.items.find((it) => it.id === "module.net")!;
  expect(net.node).toBeUndefined();
  expect(net.instances).toBe(4);
  expect(net.counts).toEqual([expect.objectContaining({ action: PlanAction.CREATE, count: 4 })]);
  expect(v.groups).toEqual([]);
  expect(v.edges).toContainEqual({ from: "terraform_data.base", to: "module.net", explicit: false });
  expect(v.edges).toContainEqual({ from: "module.net", to: "module.app", explicit: true });

  const opened = visible(g, true, new Set(["module.net"]), false);
  expect(opened.items.map((it) => it.id)).toContain("module.net.module.sub.terraform_data.leaf");
  expect(opened.groups.map((gr) => gr.id)).toEqual(["module.net", "module.net.module.sub"]);
});

test("hiding unchanged drops no-op nodes and their edges", () => {
  const v = visible(golden("actions"), false, new Set(), true);
  expect(v.items.map((it) => it.id)).not.toContain("terraform_data.keep");
  expect(v.edges.find((e) => e.from === "terraform_data.keep")).toBeUndefined();
});

test("stored positions stay, and new items do not overlap them", async () => {
  const v = visible(golden("modules"), false, new Set(), false);
  const fresh = await layout(v);
  expect(fresh.size).toBe(v.items.length);
  const stored = new Map([...fresh].slice(0, 3).map(([id, p]) => [id, { x: p.x + 1000, y: p.y }]));
  const stack = { x: 0, y: 0 };
  for (const id of [...fresh.keys()].slice(3)) fresh.set(id, stack);
  const { pos, added } = place(v.items, fresh, stored);
  for (const [id, p] of stored) expect(pos.get(id)).toEqual(p);
  expect(added).toHaveLength(v.items.length - 3);
  const all = [...pos.values()];
  for (const [i, a] of all.entries()) {
    for (const b of all.slice(i + 1)) {
      expect(Math.abs(a.x - b.x) >= nodeWidth || Math.abs(a.y - b.y) >= nodeHeight).toBe(true);
    }
  }
  const b = boxes(v, pos);
  const sub = b.get("module.net.module.sub")!;
  const net = b.get("module.net")!;
  expect(net.x <= sub.x && net.y <= sub.y && net.x + net.width >= sub.x + sub.width).toBe(true);
});

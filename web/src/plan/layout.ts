import type { default as ELK, ElkNode } from "elkjs/lib/elk.bundled.js";
import { PlanAction, type ActionCount, type PlanGraph, type PlanGroup, type PlanNode } from "../gen/stackwright/v1/graph_pb";

export const nodeWidth = 240;
export const nodeHeight = 40;
const gap = 16;
const groupPad = 16;
const groupHeader = 28;

// An item is a box on the canvas: a resource, or a module shown closed.
export type Item = {
  id: string;
  node?: PlanNode;
  action: PlanAction;
  counts: ActionCount[];
  instances: number;
  // The open group that holds it, or "".
  parent: string;
};

export type Edge = { from: string; to: string; explicit: boolean };

export type View = { items: Item[]; groups: PlanGroup[]; edges: Edge[] };

export type Point = { x: number; y: number };

export type Box = Point & { width: number; height: number };

const top = (module: string) => module.split(".").slice(0, 2).join(".");

// visible works out what to draw. With closed set, a top-level module that is
// not in open draws as one item that adds up the counts of what it holds.
export function visible(g: PlanGraph, closed: boolean, open: Set<string>, hideUnchanged: boolean): View {
  const shut = (module: string) => closed && module !== "" && !open.has(top(module));
  const items = new Map<string, Item>();
  for (const n of g.nodes) {
    if (hideUnchanged && n.action === PlanAction.NO_OP) continue;
    if (!shut(n.module)) {
      items.set(n.id, { id: n.id, node: n, action: n.action, counts: n.counts, instances: n.instances, parent: n.module });
      continue;
    }
    const id = top(n.module);
    const m = items.get(id) ?? { id, action: PlanAction.NO_OP, counts: [], instances: 0, parent: "" };
    m.action = Math.max(m.action, n.action);
    m.instances += n.instances;
    for (const c of n.counts) {
      const have = m.counts.find((x) => x.action === c.action);
      if (have) have.count += c.count;
      else m.counts.push({ ...c });
    }
    items.set(id, m);
  }
  const used = new Set<string>();
  for (const it of items.values()) for (let m = it.parent; m; m = g.groups.find((x) => x.id === m)?.parent ?? "") used.add(m);
  const groups = g.groups.filter((x) => used.has(x.id));

  const rep = (id: string) => {
    const n = g.nodes.find((x) => x.id === id);
    const module = n ? n.module : id;
    if (shut(module)) return top(module);
    if (n) return items.has(id) ? id : undefined;
    return used.has(id) ? id : undefined;
  };
  const edges = new Map<string, Edge>();
  for (const e of g.edges) {
    const from = rep(e.from);
    const to = rep(e.to);
    if (!from || !to || from === to) continue;
    const key = `${from}\n${to}`;
    edges.set(key, { from, to, explicit: e.explicit || (edges.get(key)?.explicit ?? false) });
  }
  return { items: [...items.values()], groups, edges: [...edges.values()] };
}

// ELK stays out of the main bundle. A page that draws a plan starts the
// load when it opens, so the chunk arrives while the plan does.
let elk: Promise<InstanceType<typeof ELK>> | undefined;
export const loadElk = () => (elk ??= import("elkjs/lib/elk.bundled.js").then((m) => new m.default()));

// layout returns where ELK puts each item, as absolute positions.
export async function layout(v: View): Promise<Map<string, Point>> {
  const elkNodes = new Map<string, ElkNode>();
  const root: ElkNode = {
    id: "root",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": "RIGHT",
      "elk.hierarchyHandling": "INCLUDE_CHILDREN",
      "elk.spacing.nodeNode": String(gap),
      "elk.layered.spacing.nodeNodeBetweenLayers": "48",
    },
    children: [],
    edges: v.edges.map((e, i) => ({ id: `e${i}`, sources: [e.from], targets: [e.to] })),
  };
  for (const g of v.groups) {
    elkNodes.set(g.id, { id: g.id, children: [], layoutOptions: { "elk.padding": `[top=${groupHeader},left=${groupPad},bottom=${groupPad},right=${groupPad}]` } });
  }
  for (const g of v.groups) (elkNodes.get(g.parent) ?? root).children!.push(elkNodes.get(g.id)!);
  for (const it of v.items) (elkNodes.get(it.parent) ?? root).children!.push({ id: it.id, width: nodeWidth, height: nodeHeight });

  const out = new Map<string, Point>();
  const walk = (n: ElkNode, x: number, y: number) => {
    for (const c of n.children ?? []) {
      const p = { x: x + (c.x ?? 0), y: y + (c.y ?? 0) };
      if (!elkNodes.has(c.id)) out.set(c.id, p);
      walk(c, p.x, p.y);
    }
  };
  walk(await (await loadElk()).layout(root), 0, 0);
  return out;
}

const overlaps = (a: Point, b: Point) =>
  Math.abs(a.x - b.x) < nodeWidth + gap && Math.abs(a.y - b.y) < nodeHeight + gap;

// place keeps each stored position, and puts every other item where ELK did,
// moved down until it overlaps nothing. added lists the items it placed.
export function place(items: Item[], fresh: Map<string, Point>, stored: Map<string, Point>) {
  const pos = new Map<string, Point>();
  const added: string[] = [];
  for (const it of items) {
    const p = stored.get(it.id);
    if (p) pos.set(it.id, p);
  }
  for (const it of items) {
    if (pos.has(it.id)) continue;
    const p = { ...(fresh.get(it.id) ?? { x: 0, y: 0 }) };
    while ([...pos.values()].some((q) => overlaps(p, q))) p.y += nodeHeight + gap;
    pos.set(it.id, p);
    added.push(it.id);
  }
  return { pos, added };
}

// boxes sizes each group around what it holds, inner groups first.
export function boxes(v: View, pos: Map<string, Point>): Map<string, Box> {
  const out = new Map<string, Box>();
  const depth = (id: string): number => {
    const g = v.groups.find((x) => x.id === id);
    return g?.parent ? 1 + depth(g.parent) : 0;
  };
  for (const g of [...v.groups].sort((a, b) => depth(b.id) - depth(a.id))) {
    const inner: Box[] = [
      ...v.items.filter((it) => it.parent === g.id).map((it) => ({ ...pos.get(it.id)!, width: nodeWidth, height: nodeHeight })),
      ...v.groups.filter((c) => c.parent === g.id).map((c) => out.get(c.id)!),
    ].filter((b) => b && b.x !== undefined);
    if (inner.length === 0) continue;
    const x = Math.min(...inner.map((b) => b.x)) - groupPad;
    const y = Math.min(...inner.map((b) => b.y)) - groupHeader;
    const right = Math.max(...inner.map((b) => b.x + b.width)) + groupPad;
    const bottom = Math.max(...inner.map((b) => b.y + b.height)) + groupPad;
    out.set(g.id, { x, y, width: right - x, height: bottom - y });
  }
  return out;
}

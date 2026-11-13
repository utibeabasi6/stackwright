import { TopologyRole, type Topology, type TopologyEdge, type TopologyNode } from "../gen/stackwright/v1/graph_pb";

export type Box = { x: number; y: number; w: number; h: number };

export type Column = { vpc: string; zone: string; x: number; y: number; w: number };

export type Path = { edge: TopologyEdge; d: string };

export type Layout = {
  boxes: Map<string, Box>;
  columns: Column[];
  paths: Path[];
  width: number;
  height: number;
};

export const size = {
  pad: 24,
  gap: 16,
  column: 300,
  zoneLabel: 24,
  head: 40,
  node: 32,
  groups: 200,
};

const memberRoles = new Set([
  TopologyRole.NAT_GATEWAY,
  TopologyRole.LOAD_BALANCER,
  TopologyRole.INSTANCE_GROUP,
  TopologyRole.INSTANCE,
  TopologyRole.DATABASE,
  TopologyRole.CLUSTER,
]);

const noZone = "";

// layout places each VPC as a box with its availability zones as columns,
// public subnets above private ones, and a node that spans zones on a row
// of its own under its subnets' tier. Gateways sit on the box's top edge,
// security groups in a column at its right, and what lies outside every
// VPC in a row below the boxes (adr/0038).
export function layout(t: Topology): Layout {
  const { pad, gap, column, head, node } = size;
  const boxes = new Map<string, Box>();
  const columns: Column[] = [];
  const byId = new Map(t.nodes.map((n) => [n.id, n]));
  const vpcs = t.nodes.filter((n) => n.role === TopologyRole.VPC);
  let y = pad + node * 2;
  let width = 0;

  for (const vpc of vpcs) {
    const inVpc = t.nodes.filter((n) => n.vpc === vpc.id);
    const subnets = inVpc.filter((n) => n.role === TopologyRole.SUBNET).sort(bySubnet);
    const zones = [...new Set(subnets.map((s) => s.zone || noZone))].sort((a, b) => (a === noZone ? 1 : b === noZone ? -1 : a.localeCompare(b)));
    const col = (zone: string) => Math.max(0, zones.indexOf(zone || noZone));
    const members = inVpc.filter((n) => memberRoles.has(n.role)).sort((a, b) => a.id.localeCompare(b.id));
    const within = new Map<string, TopologyNode[]>();
    const spans: TopologyNode[] = [];
    for (const m of members) {
      if (m.subnets.length === 1 && byId.has(m.subnets[0])) {
        within.set(m.subnets[0], [...(within.get(m.subnets[0]) ?? []), m]);
      } else {
        spans.push(m);
      }
    }
    const left = pad + gap;
    const top = y + head + size.zoneLabel;
    let rowY = top;
    for (const isPublic of [true, false]) {
      const tier = subnets.filter((s) => s.public === isPublic);
      const cellY = zones.map(() => rowY);
      for (const s of tier) {
        const c = col(s.zone);
        const inside = within.get(s.id) ?? [];
        const h = head + inside.length * (node + gap / 2) + gap / 2;
        const box = { x: left + c * (column + gap), y: cellY[c], w: column, h };
        boxes.set(s.id, box);
        inside.forEach((m, i) => boxes.set(m.id, { x: box.x + gap / 2, y: box.y + head + i * (node + gap / 2), w: column - gap, h: node }));
        cellY[c] += h + gap;
      }
      rowY = Math.max(rowY, ...cellY);
      for (const m of spans.filter((m) => byId.get(m.subnets[0])?.public === isPublic)) {
        const cols = m.subnets.map((s) => col(byId.get(s)?.zone ?? noZone));
        const from = cols.length ? Math.min(...cols) : 0;
        const to = cols.length ? Math.max(...cols) : zones.length - 1;
        boxes.set(m.id, { x: left + from * (column + gap), y: rowY, w: (to - from + 1) * column + (to - from) * gap, h: node });
        rowY += node + gap;
      }
    }
    const zonesW = Math.max(1, zones.length) * (column + gap);
    zones.forEach((zone, i) => columns.push({ vpc: vpc.id, zone, x: left + i * (column + gap), y: y + head, w: column }));

    const groups = inVpc.filter((n) => n.role === TopologyRole.SECURITY_GROUP);
    const rank = ranks(groups, t.edges);
    groups.sort((a, b) => rank(a.id) - rank(b.id) || a.id.localeCompare(b.id));
    const groupsX = left + zonesW + gap;
    groups.forEach((g, i) => boxes.set(g.id, { x: groupsX, y: top + i * (node + gap / 2), w: size.groups, h: node }));
    const groupsH = groups.length * (node + gap / 2);

    const w = zonesW + gap * 2 + (groups.length ? size.groups + gap : 0);
    const h = Math.max(rowY, top + groupsH) - y + gap;
    boxes.set(vpc.id, { x: pad, y, w, h });
    const gateways = inVpc.filter((n) => n.role === TopologyRole.INTERNET_GATEWAY);
    const gatewaysX = left + zonesW / 2 - (gateways.length * (size.groups + gap) - gap) / 2;
    gateways.forEach((g, i) => boxes.set(g.id, { x: gatewaysX + i * (size.groups + gap), y: y - node / 2, w: size.groups, h: node }));
    width = Math.max(width, pad + w + pad);
    y += h + pad + node;
  }

  const placed = new Set(boxes.keys());
  const internet = t.nodes.find((n) => n.role === TopologyRole.INTERNET);
  if (internet) {
    const groupEdge = t.edges.find((e) => e.from === internet.id && boxes.has(e.to));
    const target = groupEdge ? boxes.get(groupEdge.to)! : undefined;
    const vpc = target ? [...boxes.entries()].find(([id, b]) => byId.get(id)?.role === TopologyRole.VPC && b.y <= target.y && target.y <= b.y + b.h)?.[1] : undefined;
    boxes.set(internet.id, { x: target?.x ?? pad, y: (vpc?.y ?? pad + node * 2) - node * 2, w: size.groups, h: node });
    placed.add(internet.id);
  }
  const rest = t.nodes.filter((n) => !placed.has(n.id) && n.role !== TopologyRole.ROUTE_TABLE && (n.role !== TopologyRole.SECURITY_GROUP || !n.vpc));
  rest.sort((a, b) => a.id.localeCompare(b.id)).forEach((n, i) => {
    boxes.set(n.id, { x: pad + i * (size.groups + gap), y, w: size.groups, h: node });
    width = Math.max(width, pad + (i + 1) * (size.groups + gap) + pad);
  });
  const height = y + (rest.length ? node + pad : 0);

  const paths = t.edges.flatMap((edge) => {
    const a = boxes.get(edge.from);
    const b = boxes.get(edge.to);
    return a && b ? [{ edge, d: connect(a, b) }] : [];
  });
  return { boxes, columns, paths, width: Math.max(width, pad * 2), height };
}

// ranks orders security groups along their rules, so a group comes after
// the groups that reach it: the internet, then web, then app.
function ranks(groups: TopologyNode[], edges: TopologyEdge[]) {
  const ids = new Set(groups.map((g) => g.id));
  const rank = new Map<string, number>();
  const visit = (id: string, seen: Set<string>): number => {
    if (rank.has(id)) return rank.get(id)!;
    if (seen.has(id)) return 0;
    seen.add(id);
    const from = edges.filter((e) => e.to === id && ids.has(e.from) && e.from !== id).map((e) => visit(e.from, seen) + 1);
    const r = Math.max(0, ...from);
    rank.set(id, r);
    return r;
  };
  return (id: string) => visit(id, new Set());
}

function bySubnet(a: TopologyNode, b: TopologyNode) {
  return a.cidr.localeCompare(b.cidr, undefined, { numeric: true }) || a.id.localeCompare(b.id);
}

// connect draws a curve from a's edge to b's, leaving through the sides
// that face each other.
function connect(a: Box, b: Box) {
  const ac = { x: a.x + a.w / 2, y: a.y + a.h / 2 };
  const bc = { x: b.x + b.w / 2, y: b.y + b.h / 2 };
  const vertical = b.y >= a.y + a.h || a.y >= b.y + b.h;
  if (vertical) {
    const down = bc.y > ac.y;
    const from = { x: ac.x, y: down ? a.y + a.h : a.y };
    const to = { x: bc.x, y: down ? b.y : b.y + b.h };
    const mid = (from.y + to.y) / 2;
    return `M${from.x},${from.y} C${from.x},${mid} ${to.x},${mid} ${to.x},${to.y}`;
  }
  const right = bc.x > ac.x;
  const from = { x: right ? a.x + a.w : a.x, y: ac.y };
  const to = { x: right ? b.x : b.x + b.w, y: bc.y };
  const mid = (from.x + to.x) / 2;
  return `M${from.x},${from.y} C${mid},${from.y} ${mid},${to.y} ${to.x},${to.y}`;
}

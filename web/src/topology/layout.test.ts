import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fromJson } from "@bufbuild/protobuf";
import { TopologySchema } from "../gen/stackwright/v1/graph_pb";
import { layout, type Box } from "./layout";

// The Go test writes this from real plans of a two-zone VPC.
const topology = fromJson(
  TopologySchema,
  JSON.parse(readFileSync(resolve(process.cwd(), "../internal/topology/testdata/topology.json"), "utf8")),
);

const inside = (inner: Box, outer: Box) =>
  inner.x >= outer.x && inner.y >= outer.y && inner.x + inner.w <= outer.x + outer.w && inner.y + inner.h <= outer.y + outer.h;

test("a two-zone VPC lays out with zones as columns and public subnets above private ones", () => {
  const l = layout(topology);
  const box = (id: string) => {
    const b = l.boxes.get(id);
    if (!b) throw new Error(`no box for ${id}`);
    return b;
  };
  const vpc = box("network/prod/aws_vpc.main");
  const [pub0, pub1, priv0, priv1] = ["public[0]", "public[1]", "private[0]", "private[1]"].map((s) => box(`network/prod/aws_subnet.${s}`));
  expect(l.columns.map((c) => c.zone)).toEqual(["eu-west-1a", "eu-west-1b"]);
  expect(pub0.x).toBe(priv0.x);
  expect(pub1.x).toBe(priv1.x);
  expect(pub1.x).toBeGreaterThan(pub0.x);
  expect(priv0.y).toBeGreaterThan(pub0.y + pub0.h);
  for (const s of [pub0, pub1, priv0, priv1]) expect(inside(s, vpc)).toBe(true);

  expect(inside(box("network/prod/aws_nat_gateway.main"), pub0)).toBe(true);
  const igw = box("network/prod/aws_internet_gateway.main");
  expect(igw.y + igw.h / 2).toBe(vpc.y);

  const lb = box("app/prod/aws_lb.web");
  expect([lb.x, lb.x + lb.w]).toEqual([pub0.x, pub1.x + pub1.w]);
  expect(lb.y).toBeGreaterThan(pub0.y + pub0.h);
  expect(lb.y + lb.h).toBeLessThan(priv0.y);
  for (const id of ["app/prod/aws_autoscaling_group.app", "app/prod/aws_db_instance.main"]) {
    const m = box(id);
    expect([m.x, m.x + m.w]).toEqual([priv0.x, priv1.x + priv1.w]);
    expect(m.y).toBeGreaterThan(priv0.y + priv0.h);
    expect(inside(m, vpc)).toBe(true);
  }
  for (const g of ["web", "app", "db"]) {
    const b = box(`app/prod/aws_security_group.${g}`);
    expect(b.x).toBeGreaterThan(pub1.x + pub1.w);
    expect(inside(b, vpc)).toBe(true);
  }
  expect(box("internet").y + box("internet").h).toBeLessThan(vpc.y);
  expect(l.paths).toHaveLength(topology.edges.length);
});

test("a node that no VPC holds goes below the VPCs", () => {
  const l = layout(fromJson(TopologySchema, { nodes: [{ id: "tgw-1", role: "TOPOLOGY_ROLE_EXTERNAL", name: "tgw-1" }] }));
  expect(l.boxes.get("tgw-1")).toBeDefined();
});

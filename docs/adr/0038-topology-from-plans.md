# 0038 — The topology joins each workspace's latest plan across the org

**Status:** Accepted, 2026-11-13.

## Context

Phase 17 draws the AWS network with planned changes and findings on
top. BUILD.md said "from state", but projects on the code's own backend
(adr/0036) have no state in Stackwright. A VPC often lives in one
workspace and its subnets, load balancers and databases in others.

## Decision

**The source is each workspace's latest plan of its own code**: the
newest run from the UI, a push or the nightly schedule whose plan
succeeded. A plan's JSON holds the state it read (`prior_state`) and the
planned values, so one source gives what exists and what will change,
for every backend. The nightly plan (adr/0037) keeps it current. Pull
request and agent plans are left out: they show code that is not merged.

**One topology for the org.** The server reads the latest plan of every
workspace and joins the resources by AWS id: a subnet's `vpc_id` finds
the VPC whichever workspace manages it. A planned resource has no id
yet, so inside one workspace a reference in the configuration, such as
`aws_vpc.main.id`, joins it instead. An id that no plan has becomes an
external stub.

**Resource types map to roles**: VPC, subnet, internet gateway, NAT
gateway, route table, security group, load balancer, instance group,
instance, database, cluster and peering. A type with no role is not
drawn. A subnet is public when its route table sends `0.0.0.0/0` to an
internet gateway.

**Edges come from routes, security group rules and peering.** A route
runs from a subnet, through its route table, to the target. A security
group rule runs from its source (a CIDR or another group) to the group.

**The layout is our own, in zones as columns.** Each VPC is a box, each
availability zone a column, public subnets above private ones, and
gateways on the box's edge. The web app computes it from the model, so
the same model always draws the same way and a test can check positions.
The plan graph's ELK layout (adr/0015) is not reused.

**Findings and planned actions are on the nodes**: a node carries its
plan action and its open findings' count and worst severity. A planned
create draws dashed in the create colour.

## Alternatives

- **Stored state only.** Projects on the code's backend would get no
  topology.
- **One workspace at a time.** Simpler, but a split network shows as
  stubs.
- **ELK with nested boxes.** Less code, but zones do not line up and the
  layout moves when resources change.

## Consequences

- Each request reads and decrypts one plan for each workspace. That is
  fine for tens of workspaces; at hundreds it needs a stored model.
- A workspace whose latest plan failed shows its last good plan.
- Two workspaces that both manage the same AWS id show it once, from the
  first workspace by name, and the response lists the clash.
- Only AWS is drawn.

# 0039 — One topology for each project, with other projects as context

**Status:** Accepted, 2026-11-13. Supersedes the scope in
[0038](0038-topology-from-plans.md); its source, roles, edges and layout
stand.

## Context

ADR 0038 drew one topology for the whole org. The user wants one for
each project. An app project's load balancer often sits in subnets that
a network project manages, so a project alone has resources with no VPC
or zone to sit in.

## Decision

**The Topology page belongs to a project**, at
`/projects/<project>/topology`, reached from the project list. The org
page goes away.

**The server still joins every workspace's latest plan**, as ADR 0038
says, and then keeps the project's own nodes. It adds, as context, what
those nodes point at in other projects: their VPC, subnets, security
groups and route targets, and each context subnet's VPC and route
targets, so the gateways and NATs the project's subnets use show too. A context
node carries its project and no findings. An edge is kept when one end
is the project's own.

**Context draws dimmed** and names its project. Its details link to that
project's topology.

## Alternatives

- **Only the project's plans, with stubs for other ids.** The app's
  resources would float outside any VPC or zone.
- **A link in the workspace sidebar.** The user chose the project level
  only.

## Consequences

- A request still reads every workspace's latest plan in the org, as
  before.
- A project's page does not show what other projects put in its VPC. The
  network project shows its subnets empty of the app's load balancer.

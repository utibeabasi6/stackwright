# 0041 — A Terragrunt stack is a project that the real Terragrunt runs

**Status:** Accepted, 2026-11-14.

## Context

Phase 18 runs Terragrunt units. The importer of phase 15 already finds
them and leaves them out. Projects run on the backend their code names
(adr/0036), so a repository keeps working without Stackwright.

## Decision

**A stack is a project.** A stack is the folder above a set of units
that holds their parent config: `root.hcl`, or a `terragrunt.hcl` that
runs no module. A unit with neither belongs to its parent folder's
stack. The importer offers each stack as one project with the
Terragrunt layout.

**The real Terragrunt runs the units.** Terragrunt 1.2.1 is in the
executor image, pinned and checked against its release's SHA-256 sums.
A job lists the stack's units in dependency order with
`terragrunt find --dag --json`, then runs each one in that order with
`terragrunt run --tf-path <checked OpenTofu> -- plan` (or `apply`), so
every Terragrunt feature works: includes, generated files, functions
and mock outputs. OpenTofu shows each unit's plan in the folder
Terragrunt ran it in, since `show` through Terragrunt would read
dependencies' outputs again, and a unit may mock those for plan only.

**One plan for the stack.** The units' plans merge into one plan JSON
in which each unit is a module call named after its folder, as
`module.vpc`. The graph shows each unit as a group, and the security
scan and the topology read the stack as one root. The units' plan files
travel as a tar in `plan.bin`. An apply runs the units with changes, in
the same order, and stops at the first failure.

**State stays where the code puts it.** A unit uses the backend its
`remote_state` names, with the workspace's cloud credentials. A stack
on Stackwright's own state backend is refused for now: each unit needs a
state of its own, which Stackwright's state service, with one state for
each workspace, does not have.

## Alternatives

- **One project for each unit.** Simpler runs, but ordering across units
  would wait for phase 19's deployments.
- **Our own adapter that parses `terragrunt.hcl`.** More control over
  each step, but a subset of Terragrunt's features.
- **Always Stackwright's backend, with read-only tokens for
  dependencies.** The repository would then need Stackwright to plan.

## Consequences

- BUILD.md's "a unit cannot write its dependency's state" needs a state
  for each unit in Stackwright's backend. On S3 and other backends, the
  cloud account's IAM decides, not Stackwright.
- A stack's units without a `.terraform` folder of their own run in
  `.terragrunt-cache`, which the work tree that an apply gets leaves
  out; an apply downloads the module source again.
- The graph has no source links into units yet.

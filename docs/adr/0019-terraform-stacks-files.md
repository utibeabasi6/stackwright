# 0019 — Terraform Stacks files, run by Stackwright

**Status:** Accepted, 2026-11-11.

## Context

Users want to bring Terraform Stacks configurations. A Stack has component
files (`.tfcomponent.hcl`), deployment files (`.tfdeploy.hcl`) and a
`.terraform-version` file.

A spike on 2026-11-11 ran Terraform 1.17.0 in Docker on a one-component
Stack, with no HCP account:

- `terraform stacks init`, `validate`, `fmt` and `providers-lock` work
  locally.
- There is no `terraform stacks plan` or `apply`. A run starts with
  `terraform stacks configuration upload`, which needs an HCP Terraform
  organization, project and Stack. The other commands list, watch, approve
  or cancel runs in HCP Terraform.
- `terraform rpcapi` exists. Its help says it is for HCP Terraform, "is not
  intended for direct use", and "is not subject to Terraform compatibility
  promises".

HashiCorp's docs agree: Stacks is "a configuration layer in HCP Terraform".
So a newer Terraform binary in a sandbox cannot run a Stack.

## Decision

Stackwright reads Stacks files and runs them itself, after phase 19
(deployments):

- Each component instance in each deployment becomes one workspace with its
  own state.
- References from one component to another give the order. Phase 19's
  deployments run them in that order, with one approval, and pass outputs
  from one to the next as variables.
- For each component, the executor writes a small root module that calls the
  component's source with its inputs and its provider configuration.
- The project's engine runs each component: OpenTofu, or Terraform up to
  1.5.7. ADR 0001 does not change.
- A block that Stackwright cannot run yet is refused with a message that
  names it. It is never ignored.

## Alternatives

- **Drive `terraform rpcapi`.** It may hold the real Stacks engine, but I
  have not checked that, and it has no compatibility promise. Any Terraform
  release could break it.
- **Hand Stacks to HCP Terraform.** It needs an HCP account, and Stackwright
  would only show what HCP does.
- **Allow Terraform after 1.5.7 only.** It can validate Stacks files but not
  run them.
- **No Stacks support.** Users with Stacks files would have to rewrite them
  as workspaces.

## Consequences

- HashiCorp owns the file format and can change it. Stackwright follows it
  version by version, and says which version it supports.
- Results can differ from HCP Terraform at the edges. For example, I read
  that HCP Terraform can defer changes whose `for_each` is not known yet; I
  have not tested it. Stackwright plans in order instead. Each difference
  found goes into the docs.
- State in HCP Terraform does not move across. Importing a Stack that runs
  in HCP Terraform is out of scope.
- The block list in BUILD.md's phase 19b comes from HashiCorp's docs, not
  from tests. Phase 19b checks each block against real Stacks files before
  it builds on it.

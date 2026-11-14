# 0043 — A Stack's files sync into workspaces and deployments; the engine evaluates its expressions

**Status:** Accepted, 2026-11-14. Builds on [0019](0019-terraform-stacks-files.md)
and [0042](0042-deployments-and-drift.md).

## Context

ADR 0019 decided that Stackwright reads Terraform Stacks files and runs
each component itself. Before phase 19b, a check compared each block with
HashiCorp's docs, with Terraform v1.17.0's open-source Stacks parser, and
with five real Stacks repositories. It ran more than 50 cases through
`terraform stacks validate`. The findings that shape this design:

- `validate` checks only the component files (`.tfcomponent.hcl`). HCP
  Terraform parses the deployment files (`.tfdeploy.hcl`) on its servers.
  Their rules come from the docs and the language-server schema only.
- HCP Terraform plans a downstream component with unknown values and
  defers it (Terraform's deferred actions). OpenTofu has no deferred
  actions. A plain runner must apply a component before it can plan the
  components that read its outputs.
- The docs and the code disagree in several places. Where they disagree,
  Stackwright follows the code: `nullable` is not an argument, `removed`
  needs `source`, `for_each` takes a map or a set of strings, and the keys
  of `providers` are the module's provider names.
- Every real AWS example sends `identity_token.<name>.jwt` to a stack
  variable with `ephemeral = true`. A provider's `assume_role_with_web_identity`
  reads that variable.

## Decision

**The files sync into rows on import and on each push.** A Stack is a
project with the layout `stacks`. Its state is on Stackwright's backend,
one state for each workspace. A sync parses the files at the commit.
It then makes:

- one workspace for each component instance in each deployment, named
  `<deployment>-<component>` or `<deployment>-<component>-<key>`;
- one workspace named `<deployment>-outputs` when the Stack has `output`
  blocks;
- one phase 19 deployment for each Stack deployment. Its steps are those
  workspaces. Its links come from references between components, at the
  component level, as in Terraform.

A component that is gone from the files with no `removed` block fails the
sync, so no state is left without code. A deployment that the files
define cannot be changed in the web app.

**A push deploys.** After a sync, each Stack deployment gets a deployment
run. It plans the components that have the outputs they need, and it
waits for one approval for each deployment. Each component then plans
when the components it reads have applied (adr/0042). A pull request
does not plan a Stack yet.

**Stackwright evaluates only what decides the rows.** It evaluates the
deployment inputs, the `for_each` of components and providers, and the
index of a provider reference. It uses HCL's standard functions. A
`for_each` that is not known before an apply, such as one that reads
another component's output, is refused by name.

**The engine evaluates the rest.** For each component instance, the
executor writes `stackwright_stack.tf` in the Stack's folder. The file
holds the stack's variables, the locals that the component reads, one
`provider` block for each provider instance that it uses, one `module`
call with its inputs, and an output for each output of the module. Each
expression is copied from the Stack's files. The executor changes only
the names that a root module does not have:

| In the Stack | In the generated module |
| --- | --- |
| `component.<name>` | `var.stackwright_components.<name>` |
| `each` of the component | `local.stackwright_each` |
| `each` of a provider | `local.stackwright_provider_<n>` |
| `provider.<type>.<name>[<key>]` | the alias of that provider instance |

The variable `stackwright_components` holds the outputs of the earlier
components. They come from each component's last apply. The deployment
values reach the engine as `TF_VAR_` variables at plan and at apply.

**An `identity_token` is a Stackwright OIDC token.** At each plan and
apply, Stackwright mints a token with the block's audience. The provider
assumes its role itself, as in HCP Terraform. The role must trust
Stackwright's issuer, and the token's `sub` is Stackwright's, not HCP
Terraform's. The token must reach an `ephemeral` variable, so that the
saved plan does not keep it. Ephemeral variables need OpenTofu 1.11 or
Terraform 1.10. On an older engine, the plan fails with that reason.

**A `store "varset"` value is a Stackwright variable.**
`store.varset.<name>.<KEY>` reads the workspace variable `KEY` of the
block's category.

**A `removed` block plans a destroy.** Its instances stay as steps of the
deployment with `-destroy`, after every other step. A `removed` block has
no inputs, so the module gets `null` for each variable with no default.
A refresh would evaluate the resources with those nulls and fail, so the
destroy plans with `-refresh=false`. After the destroy applies, the next
sync archives the workspace. With `lifecycle { destroy = false }`, the
sync archives the workspace at once and keeps its state.

**Some blocks are refused by name, with their file and line.** A plan of
a Stack with one of these fails:

- `deployment_auto_approve`, `orchestrate`, and `auto_approve_checks`,
  `eager_plan` or `failure_tolerance` in a `deployment_group`. Every
  deployment waits for one approval.
- `publish_output`, `upstream_input` and embedded `stack` blocks.
- `store` of a type other than `varset`, and `import` or `migrate` in a
  deployment.
- `.tfcomponent.json` and `.tfdeploy.json` files.
- An `inputs` that is not an object, such as `merge(...)`. The executor
  needs the names of the inputs to write the module call.

## Alternatives

- **Read the files at run time and keep no rows.** Simpler, but the
  Deployments page and Waiting on you could not show a Stack before it
  runs.
- **Evaluate every expression in Go.** It needs Terraform's function
  library again, and the results could differ from the engine's.
- **Refuse `identity_token` and use cloud accounts.** Users would have to
  change their providers.
- **Evaluate `auto_approve_checks` on the plan's counts.** Closer to HCP
  Terraform, but an apply with no person in the loop needs more care.

## Consequences

- A Stack's first deployment applies its components one level at a time.
  HCP Terraform plans them all at once and defers the rest.
- Edges are at the component level. Every instance of `lambda` waits for
  every instance of `s3`, as in Terraform.
- A `file()` call reads from the Stack's folder, as in HCP Terraform,
  because the generated module is in that folder. A Stack folder with its
  own `.tf` files is refused.
- HashiCorp owns the format and can change it. Stackwright follows the
  format of Terraform v1.17.0 and its `tfstacks` plugin v1.4.0. The parser
  tests hold real Stacks files with that version.

# 0042 — Deployments link outputs to variables; drift is a refresh-only plan

**Status:** Accepted, 2026-11-14.

## Context

Phase 19 applies several workspaces together, in order, with one
approval, and finds changes made outside Stackwright on a schedule.
Projects run on the backend their code names (adr/0036), so Stackwright
cannot count on reading another workspace's state.

## Decision

**A deployment is defined in the web app.** It names its workspaces
(steps) and links an output of one step to a variable of another, such
as `kms.key_arn` to `secrets.kms_key_arn`. A link also orders the two
steps. A step can follow another with no link. The links make a DAG; a
cycle is refused.

**Outputs are captured after each apply.** The executor runs
`output -json` after a successful apply and uploads it. The server keeps
the newest outputs of each workspace, sealed with the org's key. A
deployment step gets its linked outputs as Terraform variables. The web
app never shows a sensitive output's value.

**A deployment run plans every step, then asks once.** Each step plans
with the outputs Stackwright has now. When every plan is done, the run
waits for one approval, from a reviewer or above. It then applies the
steps in DAG order, each when the steps before it are applied. If a
step's linked outputs changed since its plan, the step plans again and
applies without a second approval: the approval covers the deployment.
A failed step stops every step after it; steps already applied stay
applied. A destroy run plans with `-destroy` and applies in reverse
order. A step that a security finding blocks (adr/0037) stops the
approval until the finding is fixed or its risk accepted.

**A drift check is a refresh-only plan of the code last applied.**
Every 6 hours, at 00:00, 06:00, 12:00 and 18:00 UTC, each workspace that
applied at least once gets a plan-only run with trigger `drift`, of the
code of its last applied run, with `-refresh-only`. Each resource in the
plan's `resource_drift` opens a Medium finding with check `DRIFT`, which
never blocks an apply; a check that sees no drift on it closes it.
"Revert drift" starts a run of the code last applied, which waits for
approval as any run does. "Reconcile with agent" asks the agent to
change the code to match.

## Alternatives

- **A deployment file in the repository.** Reviewed in pull requests, but
  Stackwright-specific code in the user's repository.
- **Read outputs from state at run time.** Always current, but it needs
  read access to the other state, which on S3 is the cloud account's IAM.
- **Count the nightly plan's changes as drift.** Cheaper, but code that
  is waiting to apply would look like drift.
- **An interval for each workspace.** More control, one more setting.

## Consequences

- Four sandbox runs a day for each workspace that has applied.
- A step's first plan in a deployment uses the outputs from before the
  run; a changed upstream makes it plan twice.
- A workspace's outputs are as new as its last apply through
  Stackwright. An apply outside Stackwright leaves them stale until the
  next one.
- Terragrunt stacks (adr/0041) take part as one step, without outputs.

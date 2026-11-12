# 0031 — Agent plans and pull requests

**Status:** Accepted, 2026-11-12. Builds on [0011](0011-agent-session-sandboxes.md)
and [0030](0030-agent-sessions.md).

## Context

Phase 12 lets the agent plan its changes, read the result and fix them,
and ask a person to open a pull request. An agent plan comes from code that
nobody reviewed, so it must never be applied, whatever path tries.

## Decision

**An agent plan is a run marked "agent".** `run_plan` has the toolbox pack
the checkout (without `.git`, `.terraform`, state and `.env` files) and
return it on the stream. The server queues a plan-only run with trigger
`agent` and the session's id, in the workspace's normal queue, with the
workspace's variables and plan credentials. It shows in the Runs list with
its timeline and graph. The tool waits for the run and returns its summary
and its error diagnostics.

**Three locks keep it from an apply.** The database refuses an agent run
that is not plan-only or that waits for approval, applies or is applied,
and refuses any apply job whose plan belongs to an agent run. A plan-only
run never waits for approval, so nothing in the UI or the API offers to
apply it. And the agent package reaches runs, jobs, GitHub and state only
through hooks that the server sets: it imports none of the packages that
can create an apply, nor the executor, which a test checks with
`go list -deps`. The path rule that the agent shares with the toolbox
moved to `internal/toolbox/guard` for this.

**Plan and state reads are masked.** `plan_details` reads a plan's graph
and diffs with the graph package's masking: sensitive values show as
"(sensitive)", unknown ones as "(known after apply)". `state_lookup` reads
the resource from the `prior_state` of the workspace's newest plan, and
masks every path its `sensitive_values` marks. The state file alone is not
enough: its `sensitive_attributes` lists only values that the
configuration marks, not those that a provider's schema marks, such as
`random_password.result`. The plan's `prior_state` has both. So the agent
sees the state as the newest plan read it, and the answer names that run.

**A pull request needs a person.** `propose_pull_request` stores a title
and a body once the session's branch has a pushed commit and the checkout
has nothing uncommitted, and stops the turn. "Open pull request" opens it
from the session's branch to its base, ready for review, through the
installation token. Its body names the person who approved it. The
session keeps the pull request's number, and refuses a second one; later
pushes to the branch update it.

**Limits for each turn:** 40 model calls, 5 plans and 30 minutes.

**"Ask agent to fix"** on a failed node or a plan error opens the agent
panel and sends the error to the workspace's newest session that can take
a message (ready, asleep, or waiting for an answer), or starts one. It
shows to writers, on projects with a linked repository. Findings come with
phase 16.

## Alternatives

- **Agent plans as jobs outside the run queue, with `-lock=false`.** They
  would never wait for people's runs, but their results would have no run
  page or graph.
- **Draft pull requests.** A person marks them ready on GitHub, but drafts
  on private repositories need a paid plan.
- **One session for each fix.** Clean, but a fix often needs the context
  of the session that made the change.

## Consequences

- An agent plan waits behind a long apply in the same workspace.
- An agent plan reads the workspace's state with its plan credentials,
  which the workspace's runs have anyway.
- `state_lookup` needs a plan first, and shows the state as that plan read
  it, not as it is now.
- A turn that stops while its plan runs leaves the plan to finish on its
  own.

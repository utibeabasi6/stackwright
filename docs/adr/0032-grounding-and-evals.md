# 0032 — Grounding and evals

**Status:** Accepted, 2026-11-12. Builds on [0030](0030-agent-sessions.md)
and [0031](0031-agent-plans-and-pull-requests.md).

## Context

Phase 13 must make the agent write valid code the first time more often,
and show it with a score. A model guesses argument names from what it
learned, and providers change faster than that.

## Decision

**`provider_schema` reads the schema in the sandbox.** The toolbox runs
`init` without a backend, then `providers schema -json`, and returns one
resource or data source type as text: its arguments with type and
required, optional or read-only, its blocks with their limits, and the
sensitive and deprecated marks. The schemas match the provider versions
that the lock file holds. The full JSON stays in the sandbox's work folder
until the lock file changes, so later calls skip both commands. A type
that no installed provider has gets up to 10 similar names.

**`module_docs` reads the module's source after init.** The toolbox reads
`.terraform/modules/modules.json`, finds the call, and parses the module's
`.tf` files for `variable` and `output` blocks. It works for registry, Git
and local modules. The toolbox reads through an `os.Root` of the checkout,
so a symlink cannot lead it out. The agent still cannot read `.terraform`
itself.

**Conventions come from the repository, read in code.** At the start of
each turn the server asks the toolbox for the repository's
`STACKWRIGHT.md` and the project folder's, each cut at 16 KiB, and facts
from the project's code: its files, `required_version`, the
`required_providers` entries, the backend type, module calls and the
lock file's versions. They go into the system prompt after the agent's
own rules, which they cannot override. No model call writes them.

**The server refuses a tool it did not offer.** The toolbox's internal
calls (`pack`, `conventions`) are not tools, and a model that calls one
gets "There is no tool".

**The eval scores the commit the agent proposes.** `make eval` runs each
task in `evals/tasks` in a fresh Go test world with the real model, the
fake GitHub and real Docker sandboxes. The agent gets the task's prompt
and is asked to propose a commit when the plan is clean. The harness
approves the commit, plans the pushed branch as a normal plan-only run,
and checks the plan JSON against the task: resource types, counts and
planned values, and types that must not appear. A task passes only when
all of them hold. AWS tasks plan offline with fake keys and the
provider's skip flags; they use no data source that calls AWS.

**Two arms.** `ARM=baseline` hides `provider_schema` and `module_docs`;
`ARM=grounded` offers them. Both get the conventions. The harness counts
argument errors (the engine's diagnostics for an unsupported or missing
argument, block or type) in every `validate` and `run_plan` result, so
the arms can be compared on the errors as well as on the score.

**Each task has a reference solution**, which a test plans with a local
engine and no model: the solution must pass the task's checks and the
seed alone must not.

## Alternatives

- **Provider docs from the registry, on the server.** They work before
  init, but they are documentation text, not the schema, and the
  OpenTofu registry's docs API was not checked.
- **A model-written summary of the conventions.** It may catch style that
  the facts miss, but it costs a model call for each new commit.
- **Scoring by validate and file checks only.** Faster, but code that
  validates can still fail to plan.
- **Plans against the AWS lab account.** Closest to real use, but each
  eval run would need the account and real credentials.

## Consequences

- The first `provider_schema` call of a session runs `init`, which
  downloads the providers; for AWS that takes about half a minute.
- A real eval run spends money. Each run needs the user's approval and a
  stated cost.
- The module tasks use local modules, which the agent can also read with
  `read_file`; so the eval measures `module_docs` less well than
  `provider_schema`. Registry modules need data sources that call AWS,
  which the offline plans cannot run.
- The checks compare planned values, so a task cannot check a value that
  is unknown until apply.

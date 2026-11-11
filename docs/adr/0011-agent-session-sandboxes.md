# 0011 — Each agent session gets a fresh sandbox

**Status:** Accepted, 2026-11-11.

## Context

The agent edits code in a real checkout of the repo, with Git, the engine and
the provider plugins. The first build started a toolbox sandbox for each turn
(its ADR 0017), so every turn cloned the repo again. The user must stay in
control of what reaches GitHub.

## Decision

- **One fresh sandbox for each session.** A session starts a new sandbox
  through the sandbox backend ([0007](0007-sandbox-backends-and-build-servers.md)).
  No sandbox is reused between sessions.
- **The repo is cloned inside the sandbox** at the session's branch
  (`agent/<slug>`). It is not a bind mount from the build server's disk, so
  nothing from one session stays on the host for the next.
- **The sandbox lives for the session.** It stays up between turns.
- **The agent never commits or pushes on its own.** It can propose a commit:
  the files, the diff and a message. A person approves it in the
  Agent tab, and only then does the sandbox commit and push. The approver is
  the commit's committer and is recorded in the audit log. Opening a pull
  request needs the same approval.
- **Pushes go through the server's Git proxy.** The sandbox's remote is the
  server. The server lets a push through only to the session's branch and
  adds the GitHub App token itself. The token never enters the sandbox.
- **Agent plans use the working tree.** `run_plan` uploads the sandbox's
  uncommitted files as a source archive, like the CLI's speculative plan.
  Such a plan can never be applied. An apply needs a commit that a person
  approved, through the normal pull request flow.
- **The model is called from the server.** The sandbox runs tools only. It
  holds no model key, no GitHub token and no cloud credentials.
- The editor (phase 14) uses the same session sandbox, so a hand edit and an
  agent edit work on one checkout.

## An idle sandbox

- A sandbox with uncommitted changes stays up until a person commits or
  discards them. It can hold a build server slot for days.
- A sandbox with no uncommitted changes is removed after 30 minutes with no
  turn. The next message starts a fresh sandbox and clones the branch.

## Alternatives

- **Save the changes as a sealed patch in S3, then remove the sandbox.** It
  frees the slot, but it is more code, and a patch can fail to apply on a
  fresh clone.
- **Remove the sandbox and lose the changes** after a warning.
- **The agent commits and pushes at the end of each turn.** Nothing is lost
  when a sandbox is removed, but code reaches GitHub without a person's
  approval.
- **A GitHub token inside the sandbox.** Less code, but the token can push to
  every branch of the repo, and provider code that runs during `tofu init` or
  `validate` can read it.

## Consequences

- A session holds one sandbox slot while its sandbox is up. The Build servers
  page counts these slots, and "Waiting on you" lists every session whose
  changes wait for a commit.
- A session's first message waits for a sandbox start and a clone.

# 0033 — Agent sandboxes outlive a server restart

**Status:** Accepted, 2026-11-12. Builds on [0030](0030-agent-sessions.md).

## Context

The server kept where each agent sandbox runs in memory only. After a
restart it refused the toolbox that reconnected, so the toolbox exited and
the session lost its uncommitted changes. The session's row still named
the sandbox, so the next message waited 5 minutes for a toolbox that never
came, then failed. This happened on the dev stack.

## Decision

**A restarted server adopts the sandboxes it finds.** When a sandbox
starts, the session stores its platform and whether its build server is
remote. `Recover` runs before the server listens. For each session with a
sandbox token, it rebuilds what the server keeps in memory. So a
reconnecting toolbox gets its checkout order again, keeps its checkout,
and the session goes on with its uncommitted changes.

**A sandbox whose toolbox does not come back within 2 minutes is
removed**, with its token. A turn that waits for it then starts a fresh
sandbox instead of failing. A sandbox from before this change has no
stored platform and is removed at once.

**Each sandbox start has its own job id**, `agent-<session>-<random>`. The
exit of a sandbox that is no longer the session's is ignored. Before this,
the late exit of a removed sandbox could end the fresh one that replaced
it.

## Alternatives

- **Remove every sandbox at startup.** Simpler, but every restart loses
  each session's uncommitted changes.

## Consequences

- A session's uncommitted changes survive a restart only if its sandbox
  and build server do.
- A message sent in the first 2 minutes after a restart can wait that long
  for a sandbox that is gone.

# 0020 — Runs: a Postgres queue, uploaded code, joined networks

**Status:** Accepted, 2026-11-11. Supersedes the job queue in
[0003](0003-nats-inside-the-control-plane.md). For projects that a linked
GitHub installation covers, [0024](0024-git-in-sandboxes-and-linked-runs.md)
supersedes "code by upload": they run from their branch.

## Context

Phase 5 runs plans and applies. A workspace has one active run at a time,
and a job starts only when a build server has a free slot. GitHub comes in
phase 8, so a run needs another way to get code. In compose, each sandbox
sits on its own Docker network and must still reach the server and RustFS.

## Decision

**The run queue is in Postgres.** A queued run is a row. One dispatcher
goroutine in the server starts the oldest run whose workspace is free, and
starts a job when a builder has a free slot. A unique index on active runs
makes "one active run per workspace" a database rule. A `run_queue` table
holds only the ids of unfinished runs, like `job_leases`, so the dispatcher
can serve every org without row-level security.

**NATS carries only fan-out.** After a run changes, the server publishes on
`runs.<id>`. A timeline stream wakes on that message and reads Postgres. It
also reads Postgres every second, so a lost message delays an event by at
most a second.

**One timeline per run.** Orchestrator events and executor events share one
sequence per run, taken under the run's row lock. A reader that saw event n
has seen every event before it, so a reconnect with the last seq has no gap.

**Code comes from an upload or the sample.** A run takes a `.tar.gz` of the
repository, up to 32 MiB. A sample project runs the sample code built into
the server. The server seals the archive with the org's data key. The apply
uses the plan's own archive, and the executor checks its SHA-256 against the
run.

**The builder joins the server and RustFS to each sandbox's network**, by
compose service name. A sandbox reaches `http://stackwright:8080` and its
presigned URLs at `http://rustfs:9000`. Remote build servers (phase 6) use
the public URLs instead.

**A run's author may approve it.** Any member with the reviewer role or
above approves. A workspace with auto-apply needs no approval.

## Alternatives

- **The JetStream work queue that 0003 named.** "One active run per
  workspace" and "a free slot" must be checked in Postgres anyway, so the
  queue would add a second source of truth.
- **A builder that relays HTTP for its sandboxes.** It keeps the server and
  RustFS off sandbox networks, but it is more code.
- **Public URLs only.** No code, but a laptop install would need RustFS
  published where containers can reach it.
- **Sample code only until phase 8.** Less to build, but no way to try one's
  own code.

## Consequences

- One server process runs the dispatcher. Two server processes would need a
  lock on the dispatcher, for example a Postgres advisory lock.
- The builder hub keeps connected builders in memory, so a builder must
  connect to the server process that dispatches.
- A sandbox can reach the server's and RustFS's ports on the compose
  network. Egress is open already ([0018](0018-docker-sandboxes.md)).

# 0003 — NATS JetStream inside the control plane

**Status:** Accepted, 2026-11-11.

## Context

Runs need a durable job queue. The browser needs live run and agent events.
The first build used NATS JetStream without authentication, and sandboxes
could reach it. Its review found that run code could take other orgs' jobs
and read their events (first build, ADR 0032).

## Decision

NATS JetStream carries jobs and live events. The consumers run as goroutines
inside `stackwright server` ([0002](0002-one-control-plane-binary.md)), so no
separate consumer service exists.

- NATS requires authentication from phase 0. The server has the only NATS
  user. The password comes from `.env`.
- Only the control plane connects to NATS. Builders talk to the control plane
  over their own channel ([0008](0008-build-server-channel.md)). Sandboxes
  never reach NATS.
- Postgres is the record. NATS holds work in flight and fan-out only. A lost
  stream is rebuilt from Postgres.

## Alternatives

- **Postgres only** (`SKIP LOCKED` for jobs, `LISTEN/NOTIFY` for events).
  This removes one dependency. `NOTIFY` payloads are limited to 8000 bytes
  and do not survive a restart, so live events would need their own table
  and polling.

## Consequences

- Compose runs NATS with a password. The NATS port is not published.
- Phase 0 has a test that a client without the password is refused.

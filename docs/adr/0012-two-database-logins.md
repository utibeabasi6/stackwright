# 0012 — Two database logins

**Status:** Accepted, 2026-11-11.

## Context

Row-level security keeps each org's rows apart. Postgres skips it for a
superuser and for a table's owner. The Postgres image makes its first user a
superuser, and phase 0's server ran migrations and requests with that user.

## Decision

- **`stackwright` owns the schema.** It is the image's first user. Only the
  one-shot `stackwright migrate` step logs in with it.
- **`stackwright_app` serves requests.** It is not a superuser, owns no
  table and has no `BYPASSRLS`. The server logs in with it and gets no other
  database password.
- `stackwright migrate` runs the migrations, then sets `stackwright_app`'s
  password from `STACKWRIGHT_APP_DB_PASSWORD`. Compose runs it before the
  server, which waits for it to finish.
- Each request runs in one transaction that sets `stackwright.user_id` and
  `stackwright.org_id` with `set_config(..., true)`. The policies read them.

## Alternatives

- **One login with `SET LOCAL ROLE stackwright_app`.** Less setup, but the
  connection stays a superuser, so a SQL bug that runs `RESET ROLE` escapes
  row-level security.

## Consequences

- `.env` has a second database password.
- A test logs in as `stackwright_app` and checks that it cannot read another
  org's rows, even with no settings, and cannot turn row-level security off.

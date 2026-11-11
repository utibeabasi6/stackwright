# 0014 — Many orgs in one instance

**Status:** Accepted, 2026-11-11.

## Context

Some teams run one instance for several groups that must not see each
other's work.

## Decision

- An instance holds many orgs. Setup makes the first one. Instance admins
  make more.
- A user can belong to several orgs, with one role in each. The toolbar has
  an org switcher.
- Every org-scoped request names its org by slug in the request message. The
  middleware looks up the caller's membership. A missing org and an org the
  caller is not in give the same "not found".

## Alternatives

- **One org per instance.** Simpler UI. Row-level security would stay, so
  more orgs could come later without a data change.

## Consequences

- URLs start with the org slug: `/acme/projects`.
- Each test of an org-scoped call also runs it from a second org and expects
  "not found".

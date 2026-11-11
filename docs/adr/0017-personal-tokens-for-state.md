# 0017 — Personal tokens for the state backend

**Status:** Accepted, 2026-11-11.

## Context

OpenTofu and Terraform reach a workspace's state through the `http` backend.
It sends a username and a password with each request. Something must stand
behind that password.

## Decision

- **Each user makes their own tokens**, one org per token. A token has a
  name and an expiry of 1 to 365 days. It is shown once and stored as a
  SHA-256 hash. Its owner can revoke it.
- **Every request is checked against the user's role now**, not when the
  token was made. A viewer reads state. A writer, admin or owner also writes,
  locks and unlocks. A member who leaves the org, or whose role drops, loses
  that access at once.
- **One address for each workspace:** `/state/<org>/<project>/<workspace>`.
  The `http` backend has no workspaces of its own, so each Stackwright
  workspace gets a `-backend-config` file. The token goes in
  `TF_HTTP_PASSWORD`, never in a file.
- **`DELETE` is refused.** State history is kept; a workspace is archived in
  Stackwright instead.
- State writes, rollbacks and forced unlocks write audit rows, as the user.
- The CLI in phase 20 uses the same tokens.

## Alternatives

- **A token for each workspace, made by an admin.** Simpler for CI, and the
  first build did this. But a token outlives the person who made it, and it
  carries no role.

## Consequences

- A CI job uses a token of a user, or later of a service account. Service
  accounts are not planned yet.
- A token from org A gives "not found" for every address in org B.

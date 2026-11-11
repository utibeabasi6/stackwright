# 0001 — Apache-2.0 licence

**Status:** Accepted, 2026-11-11.

## Context

Stackwright is open source. People run it on their own servers. The licence
decides what others may do with the code, including hosting it for others.

## Decision

Stackwright uses the Apache License 2.0. Every source file is covered by the
`LICENSE` file at the repo root. Contributions come in under the same
licence, with a `Signed-off-by` line (DCO), not a CLA.

## Alternatives

- **AGPL-3.0.** A company that hosts a changed copy must publish its changes.
  Some companies forbid AGPL code, so fewer teams could adopt it.
- **MPL-2.0.** Changes to our files stay open, but new files can be closed.
  Fewer companies have it on their approved list than Apache-2.0.

## Consequences

- Anyone can host Stackwright for others, closed or open.
- Every dependency must have a licence that allows Apache-2.0 distribution.
  CI checks this from phase 0.
- OpenTofu (MPL-2.0) is used as a separate binary, so its licence does not
  mix with ours. Terraform 1.6 and later (BSL 1.1) is not shipped.

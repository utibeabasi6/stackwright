# 0048 — The public repository is utibeabasi6/stackwright

**Status:** Accepted, 2026-11-16.

## Context

The code goes public on GitHub. The Go module path was
`github.com/stackwright/stackwright`. The GitHub organization `stackwright`
belongs to a consulting firm that the project has no link with. A module
path in that organization cannot resolve to this code.

## Decision

The repository is `github.com/utibeabasi6/stackwright`, and the module path
is the same. The project keeps the name Stackwright.

## Alternatives

- Rename the project before it goes public. The name is the same as the
  firm's, but the firm sells consulting, not a product. A rename touches
  the code, the docs, the images and the UI. Not done now.
- Keep the old module path. Then `go install` and imports from outside
  fail. Rejected.

## Consequences

- Image names such as `stackwright/stackwright:dev` stay. They are local
  names, and adr/0047 says a builder never pulls them.
- A move to an organization later changes the module path again.

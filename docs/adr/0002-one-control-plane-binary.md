# 0002 — One control-plane binary

**Status:** Accepted, 2026-11-11.

## Context

The first build had nine services: api, state, orchestrator, graph, vcs,
agent, runner, executor and egress. Each had its own image, port, token and
compose entry. A self-hosted user had to run and upgrade all of them. The
review found bugs at the seams: the graph service trusted every caller, and
vcs and the agent service needed a shared service token.

## Decision

The control plane is one Go binary, `stackwright server`. The API, the state
backend, the orchestrator, the graph model, the GitHub integration, the agent
loop and the scanner run inside it as packages. Calls between them are Go
function calls, not network calls.

Three binaries stay separate, because they run in other places:

- `stackwright-builder` runs on a build server and starts sandboxes
  ([0007](0007-sandbox-backends-and-build-servers.md)).
- `stackwright-executor` runs inside a run sandbox.
- `stackwright-toolbox` runs inside an agent sandbox.

The `stackwright` binary also holds the admin commands (`init`,
`rotate-kek`, `user reset-password`) and, from phase 20, the user CLI
(`login`, `plan`).

A flag can switch parts of the server off (`--only=orchestrator`), so one
part can scale alone later. It is the same image and code.

## Alternatives

- **Keep separate services.** Each scales and fails alone. Self-hosting gets
  harder, and every seam needs authentication.

## Consequences

- Compose runs one Stackwright container for the control plane.
- Package boundaries replace service boundaries. Each package owns its
  tables and talks to the others through Go interfaces.
- The service token from the first build (its ADR 0034) is gone.

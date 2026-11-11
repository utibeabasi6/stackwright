# 0007 — Sandbox backends and build servers

**Status:** Accepted, 2026-11-11.

## Context

Every plan, apply and agent tool call runs in a sandbox. The first build
started Docker containers from a runner, and BYOC pools did the same in a
customer's cloud. Users want two more things: to add their own machines as
build servers, and to support more ways of making a sandbox later, such as
Unikraft unikernels.

## Decision

**A sandbox backend is a Go interface.** It starts a sandbox from a spec
(image, command, CPU, memory, time limit, network policy, files), stops it,
and reports its status and logs. Docker is the first and only backend in v1.
Unikraft comes later behind the same interface.

**A build server runs `stackwright-builder`.** The builder holds one backend,
reports its capacity and labels, and starts and stops sandboxes when the
control plane asks.

**Build servers are optional.** `docker compose up` starts one builder
container on the same host. It enrols itself with a join token from `.env`.
It is the only container with the Docker socket; the server never holds it.
So a single host runs plans with no extra setup.

**Remote build servers use the same builder and code path.** Two ways to
enrol:

1. **Over SSH.** The control plane signs in with the org's SSH key, checks
   the host key the user confirmed, installs Docker if it is missing, and
   installs the builder as a systemd service. This follows uncloud's
   `uc machine add`.
2. **A join command.** The user runs one command on the server. This covers
   machines the control plane cannot reach over SSH.

Both end with the same builder and the same client certificate
([0008](0008-build-server-channel.md)).

**Placement.** A job goes to an online build server that has a free slot and
every label the workspace requires. A draining server finishes its jobs and
takes no new ones.

**Cache.** Each builder keeps engine binaries and provider plugins in a local
folder, so jobs do not download them again. It is a cache, not storage;
blobs live in S3 ([0005](0005-s3-required-rustfs-in-compose.md)).

## Alternatives

- **The server starts sandboxes itself.** Simpler on one host, but the server
  then holds the Docker socket, which is root on the host.
- **Kubernetes Jobs as the only backend.** It suits large installs but makes
  Kubernetes a requirement.

## Consequences

- The Docker socket gives root on its host. Build servers should hold nothing
  else. The enrol page says so.
- A Kubernetes backend can be added later behind the same interface.

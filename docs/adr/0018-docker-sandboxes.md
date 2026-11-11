# 0018 — Docker sandboxes: our own API client, an engine mirror, open egress

**Status:** Accepted, 2026-11-11. Supersedes the egress proxy in
[0010](0010-sandboxes-reach-s3-by-presigned-url.md) and in `BUILD.md`.

## Context

Phase 4 runs one job in one Docker sandbox. The first build ran
`docker run -e KEY=VALUE`, so secrets showed in the host's process list and in
`docker inspect`. Each job also needs an engine binary that nobody changed.

## Decision

**Our own Docker client.** The builder calls the Docker Engine HTTP API over
the socket: networks, containers, file copy, start, wait, logs, remove. No
Docker CLI and no Docker SDK.

**Secrets are files.** The job ticket is copied into the container before it
starts. The container's environment holds no secret, so `docker inspect`
shows none. The executor reads the ticket and deletes it.

**Each sandbox is locked down:** its own bridge network, a non-root user, all
capabilities dropped, `no-new-privileges`, a read-only root file system with
`tmpfs` for work, and limits on memory, CPU and processes. The container and
its network are removed when the job ends.

**An engine mirror in S3.** The first job that needs an engine version makes
the server fetch it from the upstream release, check the checksum file's
signature (OpenTofu's and HashiCorp's keys, pinned by fingerprint) and the
archive's checksum, and store the binary in S3. A sandbox gets it by
presigned URL and checks its SHA-256 before it runs it.

**Egress is open.** A sandbox reaches the internet freely, so providers,
modules and cloud APIs work without a list. Stackwright adds no firewall
rules. The executor reaches the server through its public URL.

## Alternatives

- **The Docker Go SDK.** Complete and typed, but a large dependency tree for
  eight calls.
- **The Docker CLI.** Secrets end up in argv.
- **An egress proxy with signed grants per workspace.** It limits where run
  code can send data, but every provider, module and cloud host must be
  listed.
- **Engines straight from upstream on every job.** Less to build, but every
  job needs the upstream sites to be up.

## Consequences

- **Known gap: the control plane.** Docker on Linux drops traffic between
  bridge networks, so there a sandbox cannot reach the compose network.
  Other hosts may route it: on OrbStack, a sandbox reached Postgres, NATS
  and RustFS by their container IPs. Each still needs its own credentials.
- **Known gap: cloud metadata.** On a cloud VM, run code can reach the
  instance metadata service (169.254.169.254) and its credentials.
- Both gaps are for phase 22, with its escape tests. One way to close them
  is firewall rules on the Docker host, in the `DOCKER-USER` chain.
- The server needs outbound HTTPS to `github.com` and `releases.hashicorp.com`
  the first time a version is used.

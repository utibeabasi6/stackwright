# 0010 — Sandboxes reach S3 through presigned URLs

**Status:** Accepted, 2026-11-11. The egress proxy line is superseded by
[0018](0018-docker-sandboxes.md).

## Context

Executors download source archives and engine binaries, and upload plans and
logs. A sandbox must never hold the bucket's access key. The first build used
presigned URLs, with a relay on each sandbox network
(its `objects-gate`).

## Decision

- For each job, the orchestrator signs one URL for each object, with one
  method, valid until the job's time limit. The executor gets them in its
  claim answer.
- The executor seals each artifact with the run's artifact key before upload,
  so S3 never holds plain text.
- Each build server's egress proxy allows the host of
  `STACKWRIGHT_S3_PUBLIC_URL` and nothing else of the storage.
- With RustFS in compose and remote build servers, RustFS goes behind the
  same TLS proxy as the app, for example `s3.example.com`. A hosted store is
  already on HTTPS.

## Alternatives

- **Relay through the builder's stream.** S3 could stay private to the
  control plane, but every blob passes through the control plane, and the
  relay is our own code to test.
- **WireGuard to keep RustFS private.** It hides an endpoint that already
  needs a signed URL, at the cost in [0008](0008-build-server-channel.md).

## Consequences

- The bucket's access key never leaves the control plane.
- A leaked URL opens one object, for one method, until the job's time limit.

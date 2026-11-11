# 0005 — S3 required, RustFS in compose

**Status:** Accepted, 2026-11-11.

## Context

Stackwright stores four kinds of blobs. The first build's code shows them:

| Blob | Written by | Read by |
| --- | --- | --- |
| State versions, sealed | state backend | state backend |
| Run artifacts: `plan.bin`, `plan.json`, lock file, log, source locations | executor | apply job, graph model, UI |
| Source archives, one per commit | GitHub integration | executor |
| Engine mirror: OpenTofu binaries | setup | executor |

A plan and its apply can run on different build servers, so these blobs
need one shared store.

## Decision

Stackwright requires an S3-compatible object store and works with any of
them: AWS S3, Cloudflare R2, Garage, SeaweedFS, RustFS. It uses only `PUT`,
`GET`, `DELETE`, `LIST`, multipart upload and SigV4 presigned URLs. It does
not use bucket versioning, object lock or conditional writes. Postgres keeps
the version history.

Compose runs RustFS as the default store.

- `STACKWRIGHT_S3_URL` is the address the control plane uses.
- `STACKWRIGHT_S3_PUBLIC_URL` is the address in presigned URLs
  ([0010](0010-sandboxes-reach-s3-by-presigned-url.md)). SigV4 signs the
  host, so this must be the host that build servers reach.

## Verified and not verified

- Verified on 2026-11-11 from the RustFS README: Apache-2.0, versioned images
  (`rustfs/rustfs:1.0.5`), and a status for each feature.
- Verified on 2026-11-11 in phase 0, against `rustfs/rustfs:1.0.4`
  (`test/infra`): a presigned PUT and GET work; a presigned URL changed to
  another key is refused with 403; a 12 MiB upload in 5 MiB parts works and
  reads back the same; a wrong secret is refused. Compose uses RustFS.

## Alternatives

- **Local disk.** No extra service, but only one control-plane replica, and
  the control plane must serve every blob.
- **Postgres `bytea`.** Large plans and logs bloat the database, its WAL and
  every backup.
- **SeaweedFS.** It works and the first build used it. RustFS is one binary
  and simpler to run.

## Consequences

- CI runs the blob tests against RustFS and one more store.
- The compose default is one service. Changing it does not touch code.

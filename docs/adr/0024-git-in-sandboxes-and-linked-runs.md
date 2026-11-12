# 0024 — git in the sandbox image, and linked projects run from their branch

**Status:** Accepted, 2026-11-12. Supersedes "code by upload until GitHub"
in [0020](0020-runs-postgres-queue.md) for projects that a linked GitHub
installation covers.

## Context

The first real run of outside code failed in `init`: the registry's
`terraform-aws-modules` modules download with `git::https://`, and the
distroless executor image had no git. Most real code calls such modules.
The same run showed that a project linked to a GitHub repository still
asked for an upload.

## Decision

**The executor image is Alpine with git.** It is pinned to `alpine:3.22.2`
and adds `git` and `ca-certificates`. The sandbox still runs as 65532 with a
read-only root, no capabilities and its limits
([0018](0018-docker-sandboxes.md)). The engine's environment gets a fixed
`PATH` so it finds git. Egress was open already, so git adds no network
reach; it adds a larger program to the sandbox. Terraform's `init`, which
has no JSON output, runs with `-no-color`, so the timeline holds no color
codes.

**A linked project runs from its branch.** "Start a run" on a project that
a linked installation covers plans the newest commit of the project's
branch, or the repository's default branch, and stores its archive as a
push does. Such a project takes no upload, so no code that skipped review
can reach its applies. Projects with no repository still take an upload.

## Alternatives

- **Fetch modules outside the sandbox.** The builder or server would
  resolve and download modules with its own git, and the image would stay
  distroless. That needs a resolver for git, registry and HTTP sources, and
  a second place that does what `init` does.
- **Upload second for linked projects.** Handy for testing uncommitted
  code, but such a run could be applied without review.

## Consequences

- `apk add` installs the git that Alpine 3.22 ships when the image is
  built; it is not pinned to a package version.
- Private modules need git credentials, which a sandbox does not have yet.
- A build server that holds an older executor image has no git until it
  pulls the new one.

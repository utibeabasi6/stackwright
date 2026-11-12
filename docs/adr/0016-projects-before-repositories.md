# 0016 — Projects before repositories

**Status:** Accepted, 2026-11-11. [0025](0025-newer-terraform.md) supersedes
"Terraform 1.0.0 to 1.5.7": every Terraform from 1.0.0.

## Context

A project is a folder of infrastructure code, and a workspace is one state of
it. The GitHub App, which links repositories, comes in phase 8. Phase 3 lets a
laptop use a workspace as its state backend, which needs no repository.

## Decision

- A project has an org, a short name, a folder path, an engine and version,
  and a layout. A repository URL and branch are optional.
- Two layouts for now: a plain folder, with one `default` workspace, and CLI
  workspaces, with one workspace for each `tofu workspace`. Terragrunt comes
  in phase 18.
- Engines: OpenTofu 1.8.0 and newer, and Terraform 1.0.0 to 1.5.7. A version
  is a full `major.minor.patch`. Phase 4 checks that the engine mirror has it.
- Archive, not delete. An archived project or workspace leaves every list and
  keeps its history. Its name can be used again.
- Only admins and owners create, change or archive projects and workspaces.
- The sample project is a project with a flag. Its code ships with
  Stackwright, from phase 4.

## Alternatives

- **A repository for every project, typed in by hand.** Closer to the first
  build, but the URL does nothing until phase 4, and phase 8 replaces it.

## Consequences

- Phase 8 adds a `repos` table and links projects to it. The URL typed in
  phase 2 is a hint for that link, not a source of code.

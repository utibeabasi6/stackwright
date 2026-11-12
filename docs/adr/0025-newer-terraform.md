# 0025 — Newer Terraform, and engine versions from the release lists

**Status:** Accepted, 2026-11-12. Supersedes "Terraform 1.0.0 to 1.5.7" in
[0016](0016-projects-before-repositories.md) and the decision table's
"Terraform up to 1.5.7".

## Context

Terraform 1.6 and newer are under the Business Source Licence, and the
build plan left them for after phase 22. The user decided to support them
now. The new project form also took the engine version as free text.

## Decision

**Every Terraform from 1.0.0.** Stackwright never ships Terraform: the
engine mirror downloads each version from `releases.hashicorp.com` when a
run first needs it, and checks its signed checksums with HashiCorp's pinned
key ([0018](0018-docker-sandboxes.md)). Each install that runs Terraform
uses HashiCorp's release under HashiCorp's terms. OpenTofu stays the
default engine, from 1.8.0.

**Versions come from the release lists.** The server reads OpenTofu's
`get.opentofu.org/tofu/api.json` and HashiCorp's
`releases.hashicorp.com/terraform/index.json`, keeps the stable
`major.minor.patch` versions that Stackwright supports, newest first, and
keeps each list for an hour. The form offers them as a select and picks
the newest. If a list cannot load, the form takes a typed version.

**Terraform's init writes JSON when it can.** The executor asks the engine's
`init -help`; when it lists `-json`, init writes JSON events as OpenTofu's
does.

## Consequences

- Whether a use of Terraform 1.6 or newer meets the BUSL is for each
  install to judge. The README says so.
- The newest version is chosen by default, including one released minutes
  ago.

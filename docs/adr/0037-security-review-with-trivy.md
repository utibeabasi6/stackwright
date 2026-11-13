# 0037 — The security review runs Trivy in the sandbox

**Status:** Accepted, 2026-11-13.

## Context

Phase 16 reviews every plan and workspace for security problems, and a
High finding can block an apply. The user chose an external scanner over
rules written in Go or an embedded Rego engine.

## Decision

**Trivy scans each plan's JSON in the sandbox.** After a plan, the
executor runs `trivy config` on `plan.json` with the checks Trivy ships
with and no network (`--skip-check-update`), and uploads the result as an
artifact. Trivy 0.76.1 is in the executor image, checked against its
release checksums. A hand check on 2026-11-13 showed it raises AWS-0107
(SSH or RDP open to any address) as High on the security group's
address, offline, with a read-only root and the sandbox's user.

**An org's Rego policy sets are Trivy custom checks.** A policy set is
Rego in Trivy's check format: a `# METADATA` header with an id and a
severity, and a `deny` rule over Trivy's cloud model. The job carries the
org's sets as files, and Trivy runs them beside the built-in checks under
the `user` namespace. An org can also skip built-in checks by id, since
some fire on most code.

**A finding is one row for each workspace, check and resource address**:
its fingerprint. Plans of the workspace's own code (from the UI, a push,
or the nightly plan) open the findings they see and mark fixed those they
no longer see. A pull request's or the agent's plan changes no finding;
its run page reads its own scan report.

**High and Critical findings block, on what the plan changes.** A plan
that would wait for approval or apply on its own goes to Blocked instead
when it creates, updates or replaces a resource that has an open High or
Critical finding that no one accepted. A finding on a resource the plan
leaves alone shows on the Security tab and blocks nothing. A blocked run
holds the workspace, like one that waits for approval.

**Risk acceptance needs the security role, an admin or an owner**, with a
reason and an expiry of at most 90 days. The list is explicit: roles are
ranked by number, and writers and reviewers rank above security but may
not accept a risk. Accepting re-checks the workspace's blocked runs.
Approving checks again, so an acceptance that expired in between blocks
the apply.

**Each workspace gets a plan-only run each night**, at 02:00 UTC, which
the scan reads. It works for every backend, including the code's own
(adr/0036), and phase 19's drift check can use it. A linked workspace
plans its branch's newest commit; any other plans the code of its last
own run again. A workspace that never ran its own code and has no
repository gets none. A night on which no server runs is skipped.

## Alternatives

- **Rules in Go and Rego in an embedded OPA.** Fast and testable, but
  every built-in rule is code to write and keep current.
- **Checkov.** Many rules, but it needs Python in the executor image.
- **Scanning the stored state each night.** Cheaper, but projects on the
  code's backend have no state in Stackwright.

## Consequences

- Plans take longer by the time of a scan, a few seconds for small code.
- New Trivy releases change the built-in checks; the version is pinned
  and moves on purpose.
- The nightly plans cost a sandbox run for each workspace each night.
- Built-in checks fire on most S3 buckets (public access block, logging,
  customer-managed keys); without a skip list, many buckets block.
- A build server with an executor image from before this phase uploads no
  scan. A plan from it that would apply fails, since no review must not
  pass as a clean one; a plan-only run finishes as before.

# 0026 — Applies start from the plan's working tree

**Status:** Accepted, 2026-11-12.

## Context

The first real apply on AWS failed on a Lambda function. Its module builds
the function's zip with `data "archive_file"`, which writes the file while
OpenTofu or Terraform plans. A saved plan does not read data sources again,
so the apply expected the zip to exist. The apply ran in a new sandbox that
had only the source, `plan.bin` and the lock file, and the zip was not
there. Much real code builds files this way.

## Decision

**After a plan, the executor uploads the source root as the plan left it,**
as `workdir.tar.gz`. It leaves out every `.terraform` folder, which `init`
makes again, and `plan.bin`, which has its own artifact. It keeps folders
and regular files only, as `untar` does. The archive is sealed with the
job's key, like the other artifacts.

**The apply unpacks that archive instead of the source,** then applies the
saved plan. It makes the changes that the reviewer approved. A plan made
before this change has no such archive, so its apply falls back to the
source.

## Alternatives

- **Plan again in the apply sandbox, then compare with the approved plan.**
  The files would be written again, and the apply would see the cloud as it
  is at apply time. Each apply would take a second plan, and the comparison
  must ignore values that are unknown or that change on every plan. A wrong
  mismatch would block an apply.
- **Plan again and apply without a comparison.** The apply could make
  changes that nobody approved.
- **Leave it to the code.** Document that files a plan writes do not reach
  the apply. Common modules would fail.

## Consequences

- Files that a plan writes can hold secrets, for example from
  `local_sensitive_file`. They are sealed at rest, as `plan.bin` is.
- Each plan stores a second copy of the source in the object store.
- File modes are not kept, as with the source: every file is unpacked as
  0600, so a script that a plan writes cannot be run as a program.

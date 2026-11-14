# 0040 — Findings get plan addresses through Trivy's module names

**Status:** Accepted, 2026-11-14. Refines
[0037](0037-security-review-with-trivy.md).

## Context

ADR 0037 blocks an apply when a High or Critical finding sits on a
resource that the plan creates or changes. It matches Trivy's resource
name with the plan's addresses. Trivy 0.76.1 does not use plan
addresses:

- A root resource with `count` or `for_each` has no key:
  `aws_subnet.public`, not `aws_subnet.public[0]`.
- A resource in a module is `<type>.<name>_<md5 of module_address>`:
  `module.web.aws_security_group.this` is
  `aws_security_group.this_b9757342…`, the MD5 of `module.web`. The
  input is the plan's `module_address` as written, keys and quotes
  included.

The hash rule comes from outputs, not from Trivy's source or its
documentation. It matched all six module instances (plain, `count` and
`for_each`) in a fixture on 2026-11-14. Nested modules are not checked.

## Decision

**Each finding gets its plan address.** A root resource matches its
address without its own key. A module resource matches through the MD5
of its module address, and is stored as the address with the module's
keys, such as `module.many[0].aws_security_group.this`. It then blocks
like any other finding.

**A finding that fits no address is unmatched.** It keeps Trivy's name,
shows with a note, and never blocks an apply. That covers a Trivy
release that names resources another way.

## Alternatives

- **Show module findings unmatched and never block.** Simpler, but a
  Critical finding in a module could be applied.
- **Block on any change while an unmatched finding is open.** Safer, but
  it holds applies that do not touch the module.

## Consequences

- A Trivy upgrade can change the rule. The unit test on the module
  fixture fails if it does, and findings fall back to unmatched.
- Findings recorded under the hashed names before this close on the next
  plan, and open again under their addresses.

# 0015 — Invites are links

**Status:** Accepted, 2026-11-11.

## Context

An admin needs a way to add people. Email needs SMTP settings, which a fresh
install does not have.

## Decision

- An admin makes an invite for an email address and a role. Stackwright
  shows a one-time link once. The admin sends it.
- The link holds a random token. Postgres stores only its SHA-256 hash.
- An invite expires after 7 days. An admin can revoke it. It works once.
- Email sending comes later, with notifications.

## Alternatives

- **Email and a link.** Invites reach people without a copy step, but phase 1
  would need SMTP settings and a mail catcher in compose.

## Consequences

- No mail server in phase 1. Mailpit leaves the plan until notifications.
- A lost link means a new invite.

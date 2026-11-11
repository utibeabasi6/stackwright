# 0013 — Sign-in providers in Settings; accounts by invite only

**Status:** Accepted, 2026-11-11. Builds on
[0009](0009-local-accounts-and-oidc.md).

## Context

ADR 0009 allows local accounts and OIDC. Providers need a client secret,
which must not sit in Postgres as plain text. A self-hosted instance also
needs a rule for who may have an account.

## Decision

- **Instance admins** manage sign-in providers in Settings and create orgs.
  The first user, made at setup, is one. A way to make more comes later.
- **Two kinds of provider:** OIDC (Google, Okta, Keycloak, any issuer) and
  GitHub, which has no OIDC for user sign-in and uses OAuth 2.
- **Client secrets are sealed** with an instance data key, which the KEK
  wraps ([0004](0004-key-file-with-optional-vault.md)). So the KEK and the
  data key table arrive in phase 1, not phase 3.
- **Accounts by invite only.** No one can sign up. A sign-in reaches an
  account in one of three ways:
  1. The provider identity is already linked to a user.
  2. The provider says the email is verified, and a user with that email
     exists. The identity is then linked.
  3. The sign-in carries an open invite. A new user is made for it.
  Anything else is refused with "No account for this email. Ask an admin for
  an invite."

## Alternatives

- **Providers in `.env`.** Simpler, but a change to a provider needs a
  restart.
- **Open sign-up for anyone the provider accepts.** The first build did
  this. With Google as a provider, every Google account could join.

## Consequences

- `make init` writes `STACKWRIGHT_KEK`.
- Linking by verified email trusts the provider's `email_verified` claim.
  Add only providers whose email checks you trust.

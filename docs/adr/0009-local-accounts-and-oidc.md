# 0009 — Local accounts and OIDC, claimed with a setup code

**Status:** Accepted, 2026-11-11.

## Context

The first build allowed only GitHub and Google sign-in through OIDC. A
self-hosted install must work before anyone has set up an identity provider.
The first user to reach a fresh install would become its admin, so someone
else on the network could claim it first.

## Decision

- **Local accounts.** Email and password. Passwords are hashed with Argon2id
  and must have 12 characters or more.
- **OIDC.** GitHub, Google and any OIDC provider, added in Settings by an
  admin. SAML and SCIM come in phase 21.
- **Setup code.** On first start, the server prints a one-time setup code to
  its log. The setup page needs that code to create the first admin. The code
  stops working once the admin exists.
- Sessions, CSRF protection and roles are as in the first build: HttpOnly
  cookies, `Secure` over HTTPS, `SameSite=Lax`, and a CSRF header on every
  API call.

## Alternatives

- **OIDC only, with a bundled identity provider** such as Dex. Fewer password
  rules for us to own, but one more service to run.

## Consequences

- We own password reset. It uses the mail settings; without mail, an admin
  can reset a password with `stackwright user reset-password`.
- An org can turn local passwords off once an identity provider works.

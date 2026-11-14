# 0045 — SAML and SCIM per org; notifications and audit export through an outbox

**Status:** Accepted, 2026-11-14.

## Context

Phase 21 adds what larger orgs ask for: single sign-on, user
provisioning, notifications, and audit export. Sign-in providers so far
belong to the instance (adr/0004). The audit log is append-only and is
read only in the web app.

The user chose SAML 2.0, SSO for every member but owners, IdP groups
that map to roles, and audit export to S3 and to a webhook with a
retention of one year.

## Decision

**Each org may have one SAML identity provider.** An admin gives its
metadata, as a URL or as XML. Stackwright is the service provider at
`/auth/saml/{org}/`, with its own metadata there. A sign-in sends an
AuthnRequest with HTTP-Redirect and takes the response at the ACS with
HTTP-POST. Stackwright checks the response's signature against the
IdP's certificate, its audience, its times, and that it answers a
request Stackwright made in the last 10 minutes. It refuses encrypted
assertions. The user is the NameID, an email address.

The library is `github.com/russellhaering/gosaml2` v0.12.0. In November
2026, `crewjam/saml` fixed a critical signature bypass and several
other advisories, in releases that were then under 14 days old. Every
advisory GitHub lists for `gosaml2` is fixed by v0.11.0.

**A SAML sign-in starts a session for that org only.** The IdP posts
the response from another site, so the browser sends no Lax cookie with
it. The session that the sign-in starts records the org whose IdP signed
the user in, and opens that org only, never another org or the
instance's admin pages. A CLI token that this session approves records
the same org.

Users belong to the instance and are found by email. So an org's IdP
could assert the email of someone who also belongs to other orgs. The
scope above stops such a session from reaching those orgs. A sign-in
also matches an existing user only when they are a member of the org or
SCIM made them. An email of an account with no place in the org is
refused.

**SSO is required for every member but owners.** With SSO on, an org's
RPCs refuse a member whose session or CLI token did not sign in through
the org's IdP. They tell the member to sign in with SSO. Owners keep
their other sign-in, so an org can recover from a broken IdP. The
state backend's API tokens are not sessions. An admin makes them for the
org, so SSO does not cover them.

**SCIM 2.0 provisions users and groups.** Each org has a SCIM base URL,
`/scim/v2/{org}`, and a bearer token that an admin makes. Stackwright
serves Users and Groups with list, filter on `userName` and
`displayName`, create, replace, patch and delete, enough for Okta and
Entra ID. A SCIM user is a Stackwright user with a membership in the
org. The org maps IdP group names to roles. A user's role is the highest
role of their mapped groups, or the org's default role. A user that is
deactivated or deleted loses the membership, every session and every
CLI token. A SAML sign-in of a user with no membership adds one, with
the role from the response's group attribute.

**Notifications go through an outbox.** An admin adds Slack incoming
webhooks and HTTPS webhooks. Each destination picks its events from:
a run failed, a run waits for approval, a run applied, a High or
Critical finding opened, and drift found. The transaction that records
the event also writes a delivery for each destination. A worker sends
deliveries as soon as the transaction commits, and retries with backoff.
A webhook's body is signed with HMAC-SHA256 in
`X-Stackwright-Signature`. Destination URLs must use HTTPS.

**Audit export goes through the same kind of cursor.** An org can send
its audit rows to an S3 bucket, as JSON lines, written with one of its
cloud accounts' roles. It can also send them to a signed webhook, or to
both. A worker sends the rows after each destination's cursor every few
minutes. Rows older than the retention, 365 days unless the org sets
another, are deleted once every destination has them. With no export
set, they are deleted at the retention.

## Alternatives

- **`crewjam/saml`.** It has more features, such as IdP metadata
  signatures and encrypted assertions, but the only releases with the
  fixes were too new.
- **OIDC per org.** It reuses the instance's OIDC code, but the user
  chose SAML.
- **Send notifications from the code that records the event.** It is
  simpler, but a slow endpoint would hold the transaction, and a crash
  would lose the message.
- **A Slack app with OAuth.** It allows richer messages, but each org
  would have to install an app.

## Consequences

- An org whose IdP breaks needs an owner to turn SSO off.
- IdP metadata is fetched over HTTPS, and its signature is not checked.
  The admin who gives the URL vouches for it.
- The server posts to URLs that admins choose. It refuses other schemes
  than HTTPS, but it can reach private addresses. `docs/status.md` lists
  this as a known gap.
- A member who also belongs to other orgs signs in to each SSO org
  separately.

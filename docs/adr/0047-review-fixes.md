# 0047 — Fixes from the full review: plan tokens, SSO and SCIM limits, retention, images

**Status:** Accepted, 2026-11-15.

## Context

A review of the whole codebase on 2026-11-15 found defects in security,
correctness, performance and open-source readiness. Most fixes change no
decision. This record holds the ones that do, and the four that the user
chose from options.

## Decision

**A plan job cannot write state.** The state backend reads the job's
operation for a job token. A plan job may read the state and take and
release its own lock. Only an apply job may write. A user unlocks only a
lock that the user holds. An admin may remove another holder's lock, and
the audit log records it as `StateService/ForceUnlockState`, as the RPC
does. A state serial above 2^53 is refused, so a rollback cannot overflow.

**The engine gets a state token, not the job token.** Claim mints a
second token (`swe_`) that reaches only the job's state; the executor
gives it to the engine as `TF_HTTP_PASSWORD`. Code that a plan runs can no
longer send events or finish its job. The state backend still takes a job
token from an executor of the release before.

**Owners configure SSO, and SAML never signs in an owner.** SaveSso,
DeleteSso and RotateScimToken need the owner role. The SAML ACS refuses an
owner's email. Owners sign in another way, as adr/0045 says. Deleting SSO
ends the sessions and CLI tokens that its identity provider started. Over
https, a SAML sign-in is bound to the browser that started it, by a
`SameSite=None` cookie; over plain http browsers refuse such a cookie, so
there is no binding.

**Only the owner of a GitHub installation links it.** The installation
must be the signed-in GitHub user's own account, or an organization in
which the user is an admin. The App asks for `members: read` to check
that; an existing installation must accept the new permission.

**SCIM acts only inside its org.** SCIM links an existing account only if
it is a member of the org. An account that SCIM makes is marked as made by
that org's SCIM. Deactivation removes the membership and ends the
sessions and CLI tokens signed in through the org. It ends all of them,
and a rename changes the account's name, only for an account that this
org's SCIM made.

**Job artifacts stay 30 days.** A job of a plan-only run uploads no
plan.bin and no working tree (`Job.plan_only`). No job uploads log.jsonl,
which nothing read. A daily sweep deletes the artifacts of jobs older than
30 days. It keeps the jobs of a run that has not ended, of each
workspace's last applied run, and of each workspace's last succeeded plan
of its own code, which the topology reads. Run and event rows stay.

**The server has no default executor image, and a builder pulls only from
a named registry.** `STACKWRIGHT_EXECUTOR_IMAGE` must be set. A builder
pulls a missing image only when its name starts with a registry host, such
as `ghcr.io/` or `registry.example.com/`. A bare name such as
`stackwright/executor:dev` resolves to Docker Hub, whose `stackwright`
namespace the project does not hold, so the builder refuses it unless the
image is on the host already.

**The security role is a role set, not a rank.** A policy rule may list
the roles it admits. Accepting a risk and changing the security policy
admit the security role, admins and owners. The interceptor checks the
list, so a handler cannot forget it.

**Builders report only on jobs they were given.** The hub records which
build server got each job and toolbox. A builder's hello lists its
running jobs; the hub counts only those it gave that builder. A builder
that starts finds the sandboxes of the process before it and watches
them, and removes what a run left half made. A builder that stops leaves
its sandboxes running.

**The owner login must not be limited by row-level security.** The tables
force row-level security on their owner. Migrations, the scheduled
functions and `rotate-kek` read every org's rows. Migrate and
`rotate-kek` refuse an owner login that is neither a superuser nor has
`BYPASSRLS`.

**The API lost one RPC and gained two callers.** `GetDeploymentRun` is
gone; `GetDeployment` returns the runs. The web app archives deployments
and edits a destination's events. Each deployment decision has its own
request and response messages, as buf's lint asks; the toolbox stream is
the one exception, in `buf.yaml`.

## Alternatives

- For SSO: admins configure SSO, and SAML refuses owners only. An admin
  could still sign in as another admin. Rejected.
- For SCIM: SCIM makes new accounts only. Stricter, but a person who
  signed up first could never come under the org's SCIM.
- For retention: a per-org setting. Not needed until an org asks.
- For images: keep the default and pin it by digest. That needs a registry
  that the project holds, which it does not have yet.

## Consequences

- Admins see SSO and SCIM read-only in the web app.
- An existing SCIM link counts as not made by SCIM, so a deactivation
  ends only the sessions signed in through the org.
- A run page older than 30 days can lose its plan graph; the API says so.
- A remote build server needs the executor image loaded, or a registry
  name in `STACKWRIGHT_EXECUTOR_IMAGE`.
- The Vault Transit option that adr/0004 describes is not built. The KEK
  is the only key wrapper.

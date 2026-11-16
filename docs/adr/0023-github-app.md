# 0023 — GitHub: one App per instance from a manifest, forks never plan

**Status:** Accepted, 2026-11-12.

## Context

Phase 8 makes a pull request plan the workspaces it touches, and a merge
apply them. The first build kept GitHub in a separate `vcs` service with
its own database login; this build has one control-plane binary
([0002](0002-one-control-plane-binary.md)). Each install of Stackwright
needs its own GitHub App, because the App's webhook URL is the install's.

## Decision

**One App per instance, made from a manifest.** An instance admin starts
it in Settings › GitHub. The browser posts the manifest to GitHub, which
makes the App and sends the browser back with a code. The server trades the
code for the App's id, private key, client secret and webhook secret, and
seals them with the instance data key. The App asks for contents: write (the agent pushes branches), members:
read (to check who may link an installation, adr/0047),
pull requests: write, checks: write and metadata: read, and the push and
pull request events. The page's CSP allows a form to post to the GitHub web
URL for this.

**An org links an installation only after the installer authorizes.**
GitHub's setup redirect carries an `installation_id` that anyone can type.
So the server sends the user on to authorize the App, trades the code for
the user's token, and links the installation only if
`GET /user/installations` lists it. An installation links to one org. Each
trip to GitHub carries a one-time state bound to the user, valid for 10
minutes and spent before any work, so a failed link cannot be retried with
it. The callbacks read the session cookie without the CSRF header, which a
navigation from GitHub cannot carry; the user-bound state takes its place.

**Projects find their repository by URL.** A project's repository URL from
phase 2 ([0016](0016-projects-before-repositories.md)) becomes a generated
`owner/name` column. A webhook runs the projects whose repository it names
and whose branch, or the repository's default branch, is the event's.

**Webhooks: store, answer, then work.** The receiver checks
`X-Hub-Signature-256`, stores the payload under its `X-GitHub-Delivery`
id, and answers 202. A loop works through stored deliveries; a failure waits
30 s more on each try, five times. A redelivery has the same id and is
stored once, and a unique index allows one run per workspace, trigger,
commit and pull request.

**Which projects run.** The compare API lists the changed files (at most
300; more means every project runs). A project runs if a changed file is
under its folder, or under a local module folder (`./` or `../`) that it
calls, directly or through other modules. The server parses the module
blocks of the commit's `.tf`, `.tofu`, `.tf.json` and `.tofu.json` files
with the HCL parser.

**Pull requests plan; pushes plan and apply.** A pull request plans each
touched workspace, plan only. A push to a project's branch plans and then
applies under the workspace's rules, so `auto_apply` workspaces apply and
the rest wait for a reviewer.

**Forks never plan.** A pull request from a fork would run outside code
with the org's variables and cloud credentials. It gets one neutral
`stackwright` check that says why.

**One archive per commit.** The server downloads the commit's tarball with
an installation token, drops GitHub's top folder and its pax global header,
seals it with the org's key and stores it once at
`orgs/<org>/sources/<repo>/<commit>.tar.gz`. Every run of that commit uses
it. No GitHub token enters a sandbox.

**One check run per workspace, one comment per pull request.** A loop
creates and updates each run's check: queued, in progress, then success,
failure, action required (a plan that waits for approval), neutral
(rejected) or cancelled. A failed run's check has an annotation for each
error the engine reported, at the file's path in the repository and its
lines. The comment lists the newest commit's runs, and the loop edits it in
place. A call that GitHub refuses is tried five times for each status.

## Alternatives

- **One App per org.** Orgs would share nothing on GitHub, but each would
  repeat the setup, and the webhook path would have to name the org.
- **A separate `vcs` service**, as in the first build. It keeps the App's
  secrets out of the service that faces users, but this build is one binary,
  and the secrets are sealed like every other.
- **Trust the setup redirect.** GitHub warns that it can be forged.
- **Plan forks after a member asks, or without secrets.** Left for later;
  never planning them is the safe start.

## Consequences

- GitHub must reach `STACKWRIGHT_PUBLIC_URL` for webhooks.
  `STACKWRIGHT_GITHUB_URL` and `STACKWRIGHT_GITHUB_API_URL` point at a
  GitHub Enterprise Server instead.
- The App cannot push. The agent's branches (phase 12) need contents:
  write, which the App's owner must approve then.
- Nothing deletes old source archives yet.
- The webhook and check loops run in one server process, as the dispatcher
  does (0020).

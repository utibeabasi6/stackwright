# Status

What each phase verified, and what it left out. Update this file in the same
commit that finishes a phase.

| Phase | State |
| --- | --- |
| Design | Approved 2026-11-11 |
| 0 | Done, reviewed 2026-11-11 |
| 1 | Done, reviewed 2026-11-11 |
| 2 | Done, reviewed 2026-11-11 |
| 3 | Done, reviewed 2026-11-11 |
| 4 | Done, reviewed 2026-11-11 |
| 5 | Built, waiting for review |

## Phase 0: Repository and compose

**Verified on 2026-11-11, on macOS with Docker 29.8.2:**
- `make init` writes `.env` (mode 0600) with four random secrets, and never
  overwrites an existing one.
- `docker compose up` brings Postgres, NATS, RustFS and the server to healthy.
  `/healthz` answers `ok`, `MetaService/GetVersion` answers, and the web app
  loads from the server, deep links included.
- NATS refuses a client with no password or a wrong one. JetStream is on.
- RustFS: presigned PUT and GET, a signature refused on another key,
  multipart upload, and a wrong secret refused.
- Postgres refuses a wrong password; migrations run twice without error.
- `make lint` (golangci-lint, eslint, tsc, buf) reports nothing.
- gitleaks finds no secret in git history. Go and web licences are all on
  the allowed list.
- The web shell, in tests: the toolbar marks the current page, Settings
  shows the version a stubbed client returns, and `tokens.css` matches
  `docs/design/tokens.json`. Against the running server, `GetVersion`
  answers `{"version":"dev"}`.

**Not verified:**
- CI has not run: the repo has no remote yet.
- The page has not been looked at in a browser; only its tests ran.
- govulncheck reports standard library vulnerabilities. With the local Go
  1.26.4 there are 22. The image builds with Go 1.26.9; some fixes need Go
  1.26.10 (crypto/x509) and 1.26.11 (net/http), which are under 14 days old.

**Deferred:**
- The built-in builder to phase 4, when something uses it. Mailpit until
  notifications ([0015](adr/0015-invite-links.md)).
- The KEK in `.env` to phase 1 ([0013](adr/0013-sign-in-providers-and-accounts.md)),
  and the builder join token to phase 4.
- The command palette, the inbox count and workspace tabs to phase 2.

## Phase 1: Setup, sign-in, orgs and roles

Decisions: [0012](adr/0012-two-database-logins.md),
[0013](adr/0013-sign-in-providers-and-accounts.md),
[0014](adr/0014-many-orgs.md), [0015](adr/0015-invite-links.md).

**Verified on 2026-11-11, in Go tests against Postgres and a mock OIDC
provider:**
- Setup refuses a wrong code and makes no user; the right code makes the
  first admin and org once; a second setup is refused.
- Every RPC has a policy, and every policy names an RPC. Each org-scoped RPC
  was called as each of the six roles and matched the policy table. Anonymous
  callers get "unauthenticated", non-admins get "permission denied" on
  instance calls.
- An admin of org A gets "not found" on every org-scoped RPC for org B, and
  for B's members and invites by id. A's member list and audit log show
  nothing of B.
- Every write RPC made exactly one audit row; a failed write made none. The
  test fails if a new write RPC is not in it.
- `stackwright_app` is not a superuser and has no `BYPASSRLS`. With no
  settings it sees no orgs; in org A it sees only A. It cannot disable or drop
  a policy, switch role, or read with `row_security` off.
- OIDC sign-in with no account and no invite is refused; with an unverified
  email it is refused; with an invite it makes the account and joins the org;
  the linked identity signs in again. The client secret is sealed in Postgres.
- A forged CSRF header counts as signed out; a request from another origin is
  refused.
- Invites: weak passwords refused, a link works once, a revoked link fails,
  an existing account must sign in first, another user cannot take it.
- Owners: an admin cannot make or remove an owner; the last owner cannot step
  down.
- `stackwright user reset-password` sets a new password and ends sessions.

**Verified in the browser on the compose stack:** setup with the code from
the logs, an invite link made and accepted in a new account, the writer's
view of Members, sign-in, the audit log, and adding and removing a GitHub
provider.

**Not verified:**
- Sign-in with real GitHub, Google or any provider other than the mock.
- OIDC in the browser: the mock provider is not reachable from inside the
  server container under the address the browser uses. The Go test covers the
  flow.
- Light mode was not looked at; the browser showed dark.
- Sign-in attempts are limited per email (10 in 15 minutes) in memory, per
  server. There is no limit per client address yet; phase 22.
- A failed sign-in is not in the audit log, because the transaction rolls
  back.
- The setup code lives in the server's memory. With more than one server,
  each prints its own.

## Phase 2: Projects, workspaces and the shell

Decisions: [0016](adr/0016-projects-before-repositories.md). Open tabs are kept
in the browser, for each org. Only admins and owners change projects.

**Verified on 2026-11-11:**
- Go tests: create, list, get, update and archive projects and workspaces;
  a plain folder gets only `default`; the engine allow-list refuses
  Terraform 1.6.0, OpenTofu 1.7.9 and `latest`; a folder with `..` is
  refused; an archived project leaves every list and its name can be used
  again; the last workspace cannot be archived; the sample is named
  `sample`, then `sample-2`. The policy, role, cross-org and audit tests now
  cover the eight new RPCs.
- Web tests: a project with CLI workspaces dev, staging and prod shows three
  rows; every page is in the command palette, and choosing a workspace opens
  it; tabs survive a remount and Alt+W closes the current one; an admin with
  no projects is offered the sample.
- In the browser, light and dark: "Try the sample", the workspace Overview,
  the palette with ⌘K, opening a workspace from it, ⌥W, tabs after a reload,
  and the New project form.

**Changed from the plan:**
- ⌘T and ⌘W became ⌥T and ⌥W: browsers keep ⌘T and ⌘W for their own tabs.
- "Archiving a project with a locked workspace is refused" moved to phase 3,
  where locks arrive.
- The "Waiting on you" count appears in phase 5.

**Not verified:**
- Tabs in the browser's local storage are per browser; they do not follow a
  user to another device (by decision).
- Workspaces are listed by name, so `dev, prod, staging`, not the order they
  were typed in.

## Phase 3: State backend

Decisions: [0017](adr/0017-personal-tokens-for-state.md).

**Verified on 2026-11-11:**
- Real engines, in Docker, against the test server (`STACKWRIGHT_TEST_ENGINES=1`,
  part of `make test`): OpenTofu 1.10.7 and Terraform 1.5.7 each init with
  the backend config the server generates, apply the sample (state version
  1), fail to apply while another lock is held, and after a rollback plan the
  expected change `"one" -> "two"`.
- The protocol: no state gives 204; reads return what was written; the same
  serial and bytes again is accepted; the same serial with new bytes, another
  lineage and non-JSON are refused with 409; a second lock gives 423 with the
  holder's info; a write without the lock's ID gives 423; unlock with another
  ID gives 409; `DELETE` gives 405.
- Tokens: a viewer reads but cannot write or lock; a writer writes; a token
  from org A gets 404 in org B; a demoted writer loses write at once; a
  removed member gets 404; revoked and expired tokens get 401.
- The blob in RustFS holds neither the state's resources nor its lineage.
- Archiving a project with a locked workspace is refused, naming the lock.
- Rollback writes the old content as serial n+1, and a later write moves on
  from it. The audit test covers the four new writes; the policy, role and
  cross-org tests cover the eight new RPCs.
- On the compose stack: a token made in the UI, OpenTofu in Docker applying
  the sample twice, the Overview showing serial 2 and the history, a rollback
  from the UI, and a plan against it showing `"hello" -> "world"`.

**Not verified:**
- A write that fails after its blob reached S3 leaves an orphan blob. Nothing
  cleans those up yet.
- State over 64 MiB is refused; that limit is a guess, not measured.
- A lock has no expiry. A stuck lock needs an admin's force-unlock. From
  phase 4, a lock that a job holds goes when the job is lost.
- Terraform 1.5.7 prints its own lock info under the held lock's ID; that is
  the engine's output, not ours.
- The sample project and the form default to OpenTofu 1.10.7; OpenTofu is at
  1.13 now.

## Phase 4: Sandbox backend and executor

Decisions: [0018](adr/0018-docker-sandboxes.md).

**Verified on 2026-11-11** (Docker 29.8.2 on OrbStack, linux/arm64):
- The engine mirror checks real releases: OpenTofu 1.10.7 and Terraform
  1.5.7 pass the signature check with the pinned keys. With a fake upstream,
  a checksum file signed by another key, a changed archive, a key file that
  does not match its fingerprint, and a version with a path in it are all
  refused, and nothing is recorded. A second request does not go upstream.
- A plan of the `ok` fixture emits one `engine.planned_change` event for each
  resource, and each matches `resource_changes` in the `show -json` artifact.
- Applying that plan emits `apply_start` then `apply_complete` for each
  resource, in dependency order. The state has 2 resources, and the audit
  log names the job as the writer. A sensitive output's value is not in the
  `engine.outputs` event.
- Terraform 1.5.7 plans the same fixture. Its `init` has no `-json`, so those
  lines arrive as `engine.log`.
- The `broken` fixture fails with an error diagnostic at `main.tf` line 6.
- A changed engine binary in S3 is refused before it runs: the job fails
  with "the engine binary does not match its checksum", and no `engine.*`
  event exists.
- Killing the sandbox during an apply that holds the state lock: the lock
  went 45 s after the kill, and the job is `lost`.
- During that apply, neither `docker inspect` nor `docker top` shows the
  ticket or a job token.
- A ticket works once. A job token reads its own workspace's state, gets 404
  on another workspace, and gets 401 on the state backend and the executor
  API once its job ends. A session cookie cannot call the executor API.
- The sandbox runs as uid 65532 with no capabilities, a read-only root,
  `no-new-privileges` and the limits, and reaches the internet. Its
  container and network are gone after `Remove`.
- Source archives are unpacked through `os.Root`; entries with `..` are
  refused.

**Deferred to phase 22:**
- Egress limits. On this OrbStack host, a sandbox on its own bridge network
  connected to Postgres, NATS and RustFS by their container IPs. Docker on
  Linux drops traffic between bridge networks; I have not checked it. The
  done-when item "The sandbox cannot reach Postgres, NATS or RustFS" moved to
  phase 22's escape tests.

**Not verified:**
- Nothing in the server starts sandboxes yet. Tests create jobs and start
  sandboxes themselves; the built-in builder came in phase 5.
- Sandboxes reach the test server and RustFS through `host.docker.internal`.
  On Linux that needs RustFS published on the bridge gateway, not only on
  127.0.0.1. CI has not run.
- `plan.json` keeps the values of sensitive attributes, as `show -json`
  writes them. Redaction of variables comes with phase 7.
- Docker's default address pools hold about 30 bridge networks. More
  sandboxes at once than that fail to start.
- The cloud metadata address is reachable from a sandbox on a cloud VM
  (adr/0018), also deferred to phase 22.
- A job that fails after its source reached S3 leaves an orphan blob, as
  state does.

## Phase 5: Runs, approvals and the timeline

Decisions: [0020](adr/0020-runs-postgres-queue.md).

**Verified on 2026-11-11**, in Go tests with a real builder in the test
process, real Docker sandboxes, Postgres, NATS and RustFS:
- A second run on a busy workspace stays Queued while the first waits for
  approval, then starts when the first is applied. It plans no changes.
- A state write between plan and approval makes the approval fail the run
  with "plan is stale".
- A reviewer approves. A viewer can neither approve nor cancel.
- The approval is on the reviewer's Waiting on you and not on the viewer's,
  and leaves it when the reviewer approves.
- Canceling a run that waits for approval frees the workspace, and the
  queued run starts.
- The timeline: a viewer reads it from the start, stops after the first
  engine event, and reconnects from its last seq. The events join with no
  gap and no repeat, and match the run's last seq. The slowest engine event
  arrived 98 to 160 ms after the executor emitted it.
- A signed-out caller cannot watch a run. A builder with the wrong join
  token is refused.
- An uploaded `.tar.gz` plans; a project that is not a sample needs one, and
  an upload that is not gzip is refused.
- The audit test covers the five new writes; the policy tests cover the new
  RPCs.

On the compose stack, in the browser pane (OrbStack, dark and light):
- The built-in builder connects over HTTP/2 without TLS, and each sandbox
  reaches the server and RustFS through the joined network.
- A plan of the sample on a workspace with state plans no changes. A plan on
  a new workspace waits for approval with +1 ~0 −0, then "Approve and apply"
  applies it. The timeline shows each step live.
- The Waiting on you count shows 1, the page lists the approval, and both
  clear after a reject.

**Not verified:**
- Reloading the browser mid-run. The page uses the same stream and cursor
  that the Go test checks.
- The 2 s target was measured from when the executor emitted an event to
  when a Go client got it, not to the browser.
- Canceling a running plan or apply. The builder then stops the sandbox with
  a 60 s grace; no test covers that path yet.
- More than one reviewer at once. The list is the same for each.

**Known gaps:**
- One server process: the dispatcher and the builder hub live in its memory
  (adr/0020).
- After a builder restart, sandboxes that were running finish their jobs,
  but the builder does not remove them. Nothing cleans them up yet.
- The run timeline shows engine lines as they arrive. Phase 9 draws the
  plan as a graph.


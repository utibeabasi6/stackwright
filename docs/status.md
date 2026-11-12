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
| 5 | Done, reviewed 2026-11-12 |
| 6 | Done, reviewed 2026-11-12 |
| 7 | Built; the real AWS check waits for the user |
| 8 | Done, reviewed 2026-11-12; the real GitHub check is under way |

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

## Phase 6: Build servers

Decisions: [0021](adr/0021-build-server-enrolment.md).

**Verified on 2026-11-12**, in Go tests with the race detector, real Docker,
Postgres, NATS and RustFS:
- A container with sshd and the host's Docker socket is enrolled over SSH.
  The install copies the builder, enrols it, starts it, and runs a test
  sandbox. Three plans then run there.
- A wrong host key stops the enrolment, and the builder is not on the host.
  A host with no Docker stops it with "no Docker on", and nothing is
  installed.
- With builders labelled `region=eu` and `region=us`, a workspace that
  requires `region=eu` runs on eu. A plan already on eu finishes after eu
  starts draining. The next run waits with a `run.waiting` event and no
  build server, then runs on eu after eu resumes.
- A removed build server's stream closes within 5 s, it leaves the list, and
  its certificate is refused on reconnect.
- A builder two minor versions behind connects and shows the upgrade
  warning. Three behind, or another major version, is refused.
- A renewal on the stream returns a certificate from the CA for the same
  server, lasting 24 hours, and records it.
- `Connect` with no client certificate is refused on the server port and on
  the builder port. An enrol token works once.
- The third plan on the SSH host used the cached engine, and OpenTofu
  installed `random` from the read-only provider cache ("unauthenticated",
  which is how OpenTofu marks a filesystem mirror).
- A sandbox cannot write the cache. A sandbox with no cache sees none. A
  missing image is pulled; an image no registry has gives `ErrNoSuchImage`.
- The audit test covers the six new writes; the policy tests cover the new
  RPCs.

On the compose stack, in the browser pane (OrbStack), with a Lima VM
(`docker-rootful`, vz, aarch64, Docker 29.9.3, systemd):
- The built-in builder enrols with the join token and connects over mTLS on
  port 8443.
- The VM was enrolled over SSH from the Enrol page, as the Lima user with
  `sudo -n`, through Lima's forwarded SSH port. The fingerprint that
  Stackwright showed matched `ssh-keygen -lf` on the VM. Every step passed,
  the test sandbox too, and `stackwright-builder` is an active systemd
  service on the VM.
- With `host=lima` required on `sample › qa`, a plan ran on the VM: "on
  build server lima", the engine from the VM's cache, and the state backend
  reached at `host.lima.internal`.

**Not verified:**
- The join command script has not run on a machine. The tests cover its
  token, pin and enrolment, not the shell script.
- A renewal after a real 12 hours. The test sends the renewal message
  itself.
- On Linux Docker hosts, the tests reach the test server at
  `host.docker.internal`, which OrbStack provides.
- The VM was reached through Lima's forwarded port on the Mac, because the
  server's container on OrbStack cannot reach the VM's vzNAT address. A
  host on a real network was not tried.
- The VM had the executor image loaded by hand. No registry has it yet.

**Known gaps:**
- The cache has no size limit and no eviction.
- Providers are cached one job late (adr/0021).
- `ScanHostKey` lets an org admin make the server connect to any host and
  port. Phase 22 decides on a limit.
- One server process holds the connected builders, as with the dispatcher.
- Removing a build server does not uninstall the builder from the machine.

## Phase 7: Variables and cloud credentials

Decisions: [0022](adr/0022-variables-and-cloud-credentials.md).

**Verified on 2026-11-12**, in Go tests with the race detector, real Docker,
Postgres, NATS and RustFS, and a fake STS in the test process. The fake STS
does what AWS does: it fetches the JWKS from the server's
`/.well-known/jwks.json`, checks the signature, issuer, audience and expiry,
and then the role's trust policy as `GetAwsSetup` wrote it.
- A plan and apply on a workspace with org, project and workspace variables
  and a cloud account: the engine got the project's `greeting` over the
  org's, and a sensitive input. STS issued the plan role to the plan
  subject and the apply role to the apply subject.
- The sensitive input, a sensitive environment variable, the STS secret key
  and session token, and both web identity tokens are in none of: the job
  and run events, both `log.jsonl` files, `plan.json`, `GetRun` and
  `ListVariables`. The plan shows `hello-project:(sensitive value)`.
- A plan-phase token cannot assume the apply role.
- A second workspace on the same account, which the deployed trust policy
  does not name, fails its run with AccessDenied.
- Variables: the effective list puts workspace over project over org;
  reserved names (`AWS_*`, `TF_*` in any case, `HOME`) and sensitive values
  under 6 characters are refused; the stored row has no plain value and the
  sealed bytes do not hold it; `keepValue` changes only the description.
- Cloud accounts: roles in two accounts, the same role twice, another
  partition and a bad region are refused; the trust policies name only the
  workspaces on the account, each for its own phase; the template keeps a
  role's path; an account in use cannot be deleted.
- The issuer: the JWK thumbprint matches the example of RFC 7638; a minted
  token verifies against the JWKS; an http issuer cannot mint.
- The executor puts variables first and credentials after them, and masks
  the credentials as soon as it has them.
- The audit test covers the five new writes; the policy tests cover the new
  RPCs.

On the compose stack, in the browser pane: the Cloud accounts page warns
that the instance has no https issuer; an account was added from its form.
A sensitive variable set on `sample › qa` shows as "Sensitive, hidden", is
not in the page, and is stored sealed with no plain value. With the account
chosen, a run failed at once with "the workspace has a cloud account, but
the OIDC issuer must be an https:// URL". With it cleared, a plan ran on the
Lima build server with the variable in its claim.

**Not verified:**
- A real AWS account. The exit checks "creates a bucket" and "the plan role
  cannot write" wait for the user's run against their test account.
- AWS fetching the issuer documents. AWS needs a public https issuer, and
  the dev stack is on 127.0.0.1.
- That the generated CloudFormation template deploys.
- An environment variable reaching the engine in a real run. The executor
  test checks the environment it builds; the sample code reads no
  environment variable.

**Known gaps:**
- `plan.bin` and the state hold sensitive values, sealed at rest.
- Credentials last an hour and are not renewed.
- Only AWS.

## Phase 8: GitHub

Decisions: [0023](adr/0023-github-app.md).

**Verified on 2026-11-12**, in Go tests with the race detector, real Docker,
Postgres, NATS and RustFS, and a fake GitHub in the test process. The fake
answers the manifest exchange, installation tokens (it checks the App's
RS256 JWT with the App's key), OAuth, the user's installations, the
repositories, the compare API, tarballs built as GitHub builds them (a pax
global header and a top folder), check runs and comments.
- A pull request that changes only a module that `network` calls plans
  `network/dev` and `network/prod`, plan only, and not `app`.
- The same delivery sent twice, and a second event for the same commit,
  make one run per workspace.
- Two green checks ("Planned: +1 ~0 −0") and one comment.
- A push to the pull request that breaks `network/main.tf` turns its checks
  red, with an annotation at `network/main.tf` line 3, the line of the
  error. The comment is edited in place, not posted again, and shows the
  new commit.
- A pull request from a fork starts no run and gets one neutral
  `stackwright` check.
- A push to `main` applies `dev`, which has auto-apply, and leaves `prod`
  waiting for approval. Their checks are success and action required.
- One sealed archive for each of the three commits that ran.
- The App: only an instance admin starts the manifest, a second App is
  refused, and the callback stores it.
- Linking: an installation that the user's GitHub token does not list is
  not linked; a spent state, and another user's state, are refused; a
  failed link still spends its state.
- Webhooks: a wrong signature gets 401.
- The module walker follows local modules through `.tf`, `.tofu` and
  `.tf.json` files and nested modules, and a file that does not parse
  touches only its own folder.
- The audit test covers the five new RPC writes; the policy tests cover the
  new RPCs.
- `make licenses` passes. The allowlist now has Unicode-DFS-2016, the
  licence of the Unicode tables in `go-textseg`, which HCL needs; the user
  chose this on 2026-11-12.

On the compose stack, in the browser pane: Settings › GitHub shows the
create form to an instance admin, and a callback's error; the CSP lets a
form post to `https://github.com`; a webhook with no App gets 404, and a
signed-out callback 400; the new project form suggests linked repositories;
older runs still show their source.

**Not verified:**
- Real GitHub: the manifest form, the install and authorize redirects,
  webhooks from GitHub, and whether GitHub accepts the check annotations
  and the comment with the App's permissions. GitHub must reach the
  instance for webhooks.
- A compare of 300 files or more, which runs every project. The code path
  has no test.
- A repository rename on GitHub. Projects find their repository by the URL
  typed in Stackwright.

**Known gaps:**
- Nothing deletes old source archives.
- The App cannot push; phase 12 needs contents: write.
- One server process runs the webhook and check loops.

### Phase 8, after the first real runs

Decisions: [0024](adr/0024-git-in-sandboxes-and-linked-runs.md).

The first upload of outside code (an EKS example) failed in `init`: the
executor image had no git, which most registry modules need. Its timeline
also showed Terraform's color codes. The user then asked that linked
projects run from their branch, that the workspace's sections be tabs, and
that inputs and selects get more padding.

**Verified on 2026-11-12:**
- A plan of a module fetched with `git::https://` succeeds on OpenTofu
  1.10.7 and on Terraform 1.5.7, and no event holds a color code.
- A run started in the UI on a linked project plans the branch head that
  GitHub reports (fake GitHub), with its commit, branch and repository; an
  upload to such a project is refused. The project reports its linked
  repository and run branch.
- Web tests: the workspace opens on the Runs tab, other sections load only
  when chosen, and a linked project shows no upload field.
- The rebuilt executor image runs git 2.49.1.
- The run timeline shows an engine diagnostic as its summary with
  `file:line`, and its detail under it (web test; seen in the stored
  events of the EKS run: five "Unsupported argument" errors in
  `eks-cluster.tf`, from code written for an older EKS module).
- `TestBuildServerLabelsAndDraining` failed twice in full race runs: it gave
  a run 60 s to reach a build server, and in a full run every test world
  downloads OpenTofu at once before that. It now waits 4 minutes, as the
  other run tests do, and the server package passes.

**Not verified:**
- The tabs and the new input padding were not looked at in a browser: the
  dev stack's public URL is a tunnel, so the browser pane could not sign in.
- The upload of the EKS example again, now with git.

### Phase 8, engines and project creation

Decisions: [0025](adr/0025-newer-terraform.md).

**Verified on 2026-11-12:**
- `ListEngineVersions` reads the real release lists: Terraform 1.17.2 and
  1.5.7, and OpenTofu 1.13.9 and 1.10.7, with no pre-releases and no
  OpenTofu below 1.8.0. The list is kept for an hour (engine test with a
  fake index: one fetch for two calls).
- A project on Terraform 1.17.2 is accepted, and a plan with a git module
  succeeds on it in a real sandbox. HashiCorp's pinned key checks the
  1.17.2 release, and its `init` writes JSON, found from its help.
- A project can start with a cloud account, which every one of its
  workspaces gets; an unknown account is refused.
- Web tests: the new project form picks the newest version, offers it as
  "1.17.2 (newest)", and sends the cloud account. Settings › project uses
  the same select.
- A cache key clash: the Cloud accounts page and the new project form both
  used `cloud:<org>` for different shapes; the form now has its own key.
- The form controls were looked at on a static page with the app's styles,
  in both themes: inputs and selects are 32 px with 12 px side padding, and
  selects draw their chevron.

**Not verified:**
- A select's opened list. macOS draws it, and the browser pane cannot
  capture it.

### Phase 8, controls and plans that change build server

**Verified on 2026-11-12:**
- A plan made on one build server applies on another. The first real
  apply on AWS failed: its plan ran on the built-in builder, and the plan
  file kept that builder's state address, which the Lima VM cannot reach.
  The state addresses now go in `TF_HTTP_ADDRESS`, `TF_HTTP_LOCK_ADDRESS`
  and `TF_HTTP_UNLOCK_ADDRESS`, so a plan file holds no host. A Docker test
  plans through a host name that only the plan sandbox knows and applies
  through another, on OpenTofu and on Terraform 1.5.7. It failed before the
  change. Terraform 1.0.0 reads the address from these variables (checked
  in its container).
- Every select is the app's own select-only combobox, with keyboard,
  typeahead and labels (web tests, and in the browser pane in both themes).
- The repository field suggests the linked repositories as you type and
  takes any URL (web tests; in the browser with 161 repositories).
- A switch of engine on the new project form picks that engine's newest
  version. It used to keep the other engine's version, because `useLoad`
  returned the previous key's data while the new key loaded.
- Auto-apply and the build server labels moved from the Runs tab to the
  Settings tab (web test).

**Not verified:**
- The selects on the Members, Tokens, Variables and sign-in providers
  pages were not opened in the browser. Web tests cover the Variables and
  new project forms.
- A list near the bottom of the window opens down, past the window's edge.
- A real apply on AWS after the fix.

### Phase 8, files that a plan writes

Decisions: [0026](adr/0026-applies-start-from-the-plan-tree.md).

**Verified on 2026-11-12:**
- The second real apply on AWS reached the state and made every resource
  in its log except the Lambda function. That failed because
  `archive_file` wrote its zip during the plan, and the apply sandbox did
  not have it.
- A Docker test plans a project in `live/` whose sibling module writes a zip
  with `archive_file`, and applies a `local_file` that copies the zip. It
  failed with the same "no such file" before the change and passes after.
- A unit test packs a tree and unpacks it: `.terraform` folders, the plan
  file and a symlink are left out, and the plan's zip is kept.

**Not verified:**
- The lab's apply on AWS with this change.

## Phase 9: Plan graph

Decisions: [0027](adr/0027-plan-graph.md).

**Verified on 2026-11-12:**
- Golden graphs from real plans, in Docker, with OpenTofu 1.10.7: the
  sample project, an `actions` fixture applied then changed, and a
  `modules` fixture. The `actions` plan from Terraform 1.5.7 gives the same
  graph as OpenTofu's.
- Every action: create, update, replace, replace with
  `create_before_destroy`, destroy, read (a data source that waits for a
  resource) and no change, each in the `actions` golden graph.
- Edges follow module outputs, module arguments (`var`), locals, `for_each`
  and `count`; `depends_on` edges are marked explicit; `depends_on` on a
  module joins its group. Nested modules are nested groups, and a module
  with `count` is one group.
- Diffs: a change, an unknown value, a sensitive value shown as
  "(sensitive)", a path that forces replacement, a create without its null
  attributes, and a destroy (graph unit tests on the stored plans).
- The executor maps resources, module calls and locals to files with the
  HCL parser, for the root module and every module in `modules.json`.
- `GetPlanGraph` with action and module filters, `GetPlanNode`, and
  `SaveGraphLayout` (merge and replace, writers only, audited), through a
  real run on the sample project.
- Playwright, in light and dark, with the API mocked by the golden graphs:
  each node is reached with Tab and is announced with its action ("…,
  destroy, 1 instance"); arrow keys move between nodes; Enter shows the
  diff and a GitHub link to the file and line; a 300-node plan draws in
  about 220 ms (limit 1 s); a 400-node plan opens as 4 closed modules, and
  Enter opens one; a writer's browser saves a new layout once, and a stored
  position is kept; a viewer's browser saves nothing.
- On the dev stack, in the browser pane in both themes, with the AWS lab's
  real plans (#3 and #4, 28 resources in 2 modules): the summary line,
  search, Enter on the Lambda function and its create diff, and a node with
  2 instances.

**Not verified:**
- File links on a real run: plans #3 and #4 were made before the executor
  wrote `sources.json`. The Lima VM needs the new executor image for it.
- The `aws-bucket` fixture has no golden graph: it plans only with AWS
  credentials.
- A plan with thousands of resources. The closed view was measured with 400.
- Screen reader output. The checks read the accessible names, not speech.

**Known gaps:**
- Edges are drawn by React Flow, not along ELK's routes, so on a large
  graph some cross nodes.
- A stored layout keeps positions for resources that later leave the plan.
- `.tf.json` files are not parsed, so their resources have no links.

## Phase 10: Live apply and replay

Decisions: [0028](adr/0028-live-apply-and-replay.md).

**Verified on 2026-11-12:**
- The golden test now applies two plans in Docker with OpenTofu 1.10.7 and
  keeps their events: `actions`, which applies every kind of change, and
  `failing`, whose `local_file` cannot write and fails.
- From those real events (unit tests): each node reaches done at its last
  completion, in event order; a replace counts its create and its delete;
  a failed node holds its diagnostic (`main.tf:10`), and the node after it
  stays waiting; a stream fed in overlapping pieces ends in the same state
  as one stream; a replay matches the live state after every event; replay
  gaps are cut to 2 s.
- The TypeScript and Go key stripping pass the same cases from
  `internal/graph/testdata/keys.json`.
- Playwright, in light and dark: during a live apply the stream drops, the
  page resumes after the last sequence it had, and every node ends done; a
  finished apply scrubs back to all waiting, replays at 10x, and ends with
  the same node states as the live run; a failed node announces "failed"
  and Enter shows its diagnostic.

**Not verified:**
- A live apply on the dev stack in the browser pane. The browser checks
  stream recorded events from a real apply through a mocked API.
- An apply of many resources over minutes, and replay of it.

**Known gaps:**
- After a failed apply, nodes that never started stay "waiting".
- An apply that ran twice shows only its latest attempt on the graph.

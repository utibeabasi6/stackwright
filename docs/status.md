# Status

What each phase verified, and what it left out. Update this file in the same
commit that finishes a phase.

| Phase | State |
| --- | --- |
| Design | Approved 2026-11-11 |
| 0 | Done, reviewed 2026-11-11 |
| 1 | Done, reviewed 2026-11-11 |
| 2 | Done, reviewed 2026-11-11 |
| 3 | Not started |

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

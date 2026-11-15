# Threat model

This doc lists what Stackwright protects, from whom, and where the
protection stops. Read it before you change sandboxes, tokens, or
anything that a request can reach. The operator's side is in
[operations.md](operations.md).

## What we protect

- **Cloud credentials.** Variables, OIDC tokens for AWS, and the
  credentials that runs use. With them, an attacker controls the
  customer's infrastructure.
- **State.** It often holds secrets in plain text, such as database
  passwords.
- **Control of applies.** Who can make a change happen in the cloud.
- **Isolation between orgs.** One org must not read or change another
  org's data.
- **The host.** The control-plane host and the build server hosts.

## Who attacks

1. **A member of an org,** within their role. They can write code that
   runs in a sandbox: a module, a provider, an external data source.
2. **Code from outside,** in a module or provider that a member uses
   without reading. It runs with the same access as the member's code.
3. **A pull request author** on a linked repository, who need not be a
   member. A pull request triggers a plan.
4. **The agent,** which writes code and can be led by text in the
   repository or in provider docs.
5. **Someone on the network** with no account, who can reach the
   server's public ports.

## Boundaries and what holds them

### Between orgs

- Postgres row-level security on every org table. The server sets the
  org for each transaction. The app's login is not a superuser and cannot
  bypass row-level security. A function that must cross orgs is
  `SECURITY DEFINER` and does one job.
- Each org has its own data key. The KEK wraps the data keys, and the
  KEK is not in the database.
- A SAML session works only in the org of its identity provider.

### Inside an org

- Roles decide who can do what. The database refuses an apply that the
  agent or the CLI started; those runs only plan.
- An apply needs an approval, unless the workspace applies on its own.
- The audit log records every change. It can be exported.

### Sandboxes

Each plan, apply, agent turn and editor session runs in its own Docker
container:

- It runs as UID 65532 with no capabilities and `no-new-privileges`. Its
  root file system is read-only. `/work` and `/tmp` are size-limited
  tmpfs. Memory, CPU and processes are limited.
- Its network is internal. Its only way out is the egress proxy, which
  allows public addresses and the server URL of its order. It refuses
  private, loopback, link-local, shared and multicast addresses. The
  proxy checks each address after it resolves the name, and dials the
  address that it checked.
- It gets a token for its job only. The token ends with the job.
- Object storage is reached through presigned URLs, one for each object.

`TestSandboxesCannotEscape` in `internal/server/escape_test.go` checks
the network rules. A sandbox fails to reach another sandbox, the
metadata service, the Docker bridge gateway, the builder port, Postgres,
NATS and RustFS, both directly and through the proxy. It reaches the
server's job API and a public host.

### Build servers

- A build server holds a certificate from the builder CA that lasts a
  day. It connects out to the server; the server never connects in.
- A remote build server's join token works once.

### The public API

- Rate limits per caller: see [operations.md](operations.md#rate-limits).
- Cookies are `HttpOnly` and `SameSite=Lax`, and `Secure` behind https.
  Requests that change state need the CSRF header.

## Where the protection stops

These are known. Each one needs a decision before a change can close it.

- **Docker is the sandbox.** Sandboxes share the host's kernel. A kernel
  bug can let code out of a sandbox. A microVM would close this.
- **The builder holds the Docker socket,** which is root on its host.
  Anyone who controls the builder controls the host.
- **A sandbox can send what it has to any public host.** Code in a
  module or provider can read the run's cloud credentials and send them
  out through the proxy. The proxy limits where a sandbox can go, not
  what it sends.
- **A pull request plan runs the author's code** with the workspace's
  plan credentials. Give plans read-only credentials.
- **The server connects where an org admin tells it.** Notifications go
  to any https address, and `ScanHostKey` connects to any host and port,
  private ones included. An org admin can make the server send a request
  into the server's own network.
- **SSO is enforced on the API only.** The Git proxy, the editor's
  language server and the state backend's API tokens do not check it.
- **Rate limits count in each server process.** Two servers allow twice
  as much.
- **No pen test has run.**

## Changing this

When a change moves a boundary, update this doc in the same change. If
it adds a way out of a sandbox, add a target to the escape test.

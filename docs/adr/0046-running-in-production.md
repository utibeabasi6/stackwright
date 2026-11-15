# 0046 — Sandboxes reach out only through an egress proxy; keys rotate online

**Status:** Accepted, 2026-11-14.

## Context

Phase 22 makes Stackwright fit to run for real. Before it, the built-in
builder joined the `stackwright` and `rustfs` containers to each
sandbox's network. A sandbox could then reach every port of the server's
container and the object store's API. It could also reach any address
its bridge network routes to: the host, the cloud's metadata service and
private networks.

The user chose an egress proxy, rate limits by user and by IP, online
KEK rotation with two keys, and an upgrade test from the release before.

## Decision

**A sandbox's network is internal.** It has no route out. The builder
starts an egress proxy container from the executor image, with
`/stackwright-executor egress-proxy`. The proxy sits on the builder's
own networks, or on the default bridge when the builder is not in a
container. The builder connects it to each sandbox's network as
`stackwright-egress`, port 3128. The sandbox gets `HTTPS_PROXY` and
`HTTP_PROXY` for that address. OpenTofu, Terraform, Terragrunt,
providers, Git over HTTPS and the executor all use them. One proxy
serves every sandbox with the same allow list. The builder removes it
when no sandbox network is left.

The proxy resolves each target once and dials the address it checked.
It refuses private, loopback, link-local (the metadata service), shared
(100.64.0.0/10) and multicast addresses, IPv4 and IPv6. The exceptions
are the server URL that each order names, at its own host and port. Those are the server's job API, and they can be private.
The proxy logs each refusal.

**The server relays object storage.** A sandbox gets presigned URLs for
its inputs and uploads. The server signs them for its own address, the
sandbox URL, under `/s3/`. It then forwards such requests to the object
store with the Host header unchanged, so the signature still holds. It
forwards only GET, HEAD and PUT on its bucket. With
`STACKWRIGHT_S3_PUBLIC_URL` set, such as for AWS S3, sandboxes go there
through the proxy instead.

**The API is rate limited in the server.** Token buckets limit each
signed-in user, and each client IP on the routes that need no sign-in:
password sign-in, invites, the device flow and SAML. SCIM has its own
limit per client IP. Too many requests get 429, or `resource_exhausted`
over Connect, with `Retry-After`. The limits are 5 a second with a burst
of 30 for sign-in, 50 with a burst of 300 for the API, and 20 with a
burst of 200 for SCIM. Behind a proxy, the client IP comes
from `X-Forwarded-For`, but only when the request comes from an address
in `STACKWRIGHT_TRUSTED_PROXIES`. Jobs, builders and the state backend
are not limited here: their tokens are per job or per workspace.

**Keys rotate online.** The server takes `STACKWRIGHT_KEK` and, while a
rotation runs, `STACKWRIGHT_KEK_PREVIOUS`. It opens data keys that
either KEK wrapped, and wraps new ones with the current KEK.
`stackwright rotate-kek` re-wraps every data key that the previous KEK
wrapped, in one transaction. After it, the previous KEK can go. The
built-in builder's join token works the same way, with
`STACKWRIGHT_BUILDER_TOKEN_PREVIOUS`. Remote build servers' join
tokens work once, and build servers' certificates already last a day and
renew on their stream.

**An upgrade keeps the release before working.** A test builds the
last release tag, or `STACKWRIGHT_UPGRADE_FROM`, from Git with Compose.
It starts that server, the builder and the executor, makes a run that
waits for approval, and replaces only the server with the working tree.
The run then applies on the old builder and executor. Server N accepts
builders and executors of N-1.

## Alternatives

- **Firewall rules on the host.** Lighter, but they need NET_ADMIN on
  the host, and Docker Desktop does not allow them.
- **The proxy in the builder process.** No extra container, but a
  remote builder runs on its host, not in a container, so it cannot join
  a sandbox's internal network.
- **Rate limits only in the TLS proxy.** That would add nothing to
  Stackwright, but an install without such a proxy would have none.
- **Offline KEK rotation.** Simpler, but it needs a short outage.

## Consequences

- A provider that speaks something other than HTTP or HTTPS, such as a
  database provider or an SSH provisioner, cannot reach its target from
  a sandbox.
- The proxy sees the hosts that sandboxes connect to. TLS stays end to
  end, because HTTPS goes through CONNECT.
- Rate limit counts live in each server process. Two servers each count
  their own requests.

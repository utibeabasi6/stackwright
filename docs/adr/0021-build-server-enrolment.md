# 0021 — Build servers: a builder port with its own CA, a binary install, a builder-filled cache

**Status:** Accepted, 2026-11-12. Supersedes the port 443 and trusted
certificate lines of [0008](0008-build-server-channel.md), and the Docker
install line of [0007](0007-sandbox-backends-and-build-servers.md).

## Context

Phase 6 lets a user enrol their own machines. Four questions had no answer in
0007 and 0008:

1. Where the server checks a builder's client certificate. The server serves
   plain HTTP on 8080, and the README puts a TLS proxy in front. A proxy that
   ends TLS cannot check a client certificate for us.
2. How a remote machine gets the builder and the executor. No images or
   releases are published yet, and a later sandbox backend, such as
   Unikraft, cannot take a Docker image that the builder hands it.
3. How a cache of engines and providers stays safe. A cache that sandboxes
   can write lets one job plant a changed provider that a later job runs.
4. What enrolment does on a host with no Docker.

## Decision

**A builder port with Stackwright's own CA.** The server serves builders on
a second port, 8443, with TLS from a CA that it makes on first start. The CA
key is sealed with the instance data key. The port asks for a client
certificate but does not require one at the handshake, because `Enrol` comes
before the builder has one. The interceptor requires a certificate for
`Connect`, and checks its serial in `build_server_certs` on every new
stream. A builder trusts the port by the CA's SHA-256, which the join
command and the SSH install carry. A proxy in front of 8080 stays as it is;
8443 needs TLS passthrough or its own published port.

**Short certificates, renewed on the stream.** A certificate lasts 24 hours.
After half of that the builder sends a new CSR on its stream and gets a new
certificate back. The server closes a stream when its certificate ends, and
the builder connects again with the new one. Removing a build server
revokes its certificates and closes its stream in the same request.

**Enrolment spends a one-time token.** A join command or an SSH install
makes a token for one build server in one org, valid for an hour. The
built-in builder enrols with the join token from `.env` at each start, over
the server's own port inside compose, and serves every org. Its row has no
org.

**The builder is a static binary with a systemd unit.** The server image
carries `stackwright-builder` for amd64 and arm64 and hands it out on the
builder port. The SSH install copies it over the SSH session and checks its
SHA-256. The join command downloads it with the CA pinned and checks the
same SHA-256. The builder needs nothing on the host but its sandbox backend,
so a backend with no Docker can use the same install.

**Each backend gets its executor from a registry.** The server names the
executor image for each sandbox backend (`STACKWRIGHT_EXECUTOR_IMAGE` for
Docker) and sends it in each start order. The Docker backend pulls the image
when the host lacks it. A builder that reports a backend the server has no
image for is refused.

**Only the builder writes the cache.** The builder downloads each engine by
presigned URL, checks its SHA-256, and keeps it under that hash. Job
sandboxes mount the cache read-only, and the executor checks the SHA-256
again before it runs the engine. For providers, the server learns them from
each plan's lock file. The next start order lists them, and the builder runs
the engine's `providers mirror`, which checks provider signatures, in a
sandbox that runs no job code and may write the cache. Job sandboxes use the
cache as a filesystem mirror, before the registry.

**No Docker means no enrolment.** The SSH install checks for Docker and for
systemd before it changes anything. Without Docker it stops and says so. It
never installs packages on the user's machine.

**Placement.** A job goes to the least busy online build server that serves
its org, has a free slot, is not draining, and has every label the workspace
requires. An apply goes to the platform of its plan. A run with no such
server waits, and its timeline says what it waits for.

**Versions.** A builder with the server's major version and at most two
minor versions behind connects, with an upgrade warning when it is older. A
builder further behind, or of another major version, is refused.

## Alternatives

- **TLS on the main port with a public certificate.** It follows 0008, but
  the server must own its public TLS, and a proxy in front must pass TLS
  through for every request, not only for builders.
- **The server hands out the executor image as a tarball.** It works
  offline, but only for Docker. A unikernel backend cannot load it.
- **The builder as a container.** It needs Docker on the host, so it ties
  the install to one backend.
- **A writable cache for each workspace.** Simpler, but a workspace can
  still poison its own later runs.
- **Installing Docker when it is missing.** It needs root and a tested
  package path for each distribution, and it changes a machine the user did
  not ask us to change.

## Consequences

- Remote build servers need the builder port (8443) and S3 reachable, and
  their sandboxes need the server's URL. `STACKWRIGHT_BUILDER_URL`,
  `STACKWRIGHT_SANDBOX_URL` and `STACKWRIGHT_S3_PUBLIC_URL` name them.
- The hub keeps connected builders in memory, so one server process holds
  them all, as with the run dispatcher (0020).
- The cache has no size limit and no eviction yet.
- Providers are cached one job late: the first plan of a workspace teaches
  the server its providers, the next start order fills the cache, and the
  plan after that uses it.
- `ScanHostKey` makes the server connect to any host and port an org admin
  names. It reads only the SSH key exchange, but it is a network probe from
  the server. Phase 22 decides whether to limit it.

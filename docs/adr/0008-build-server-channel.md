# 0008 — Build servers dial out over gRPC with mTLS

**Status:** Accepted, 2026-11-11. [0021](0021-build-server-enrolment.md)
supersedes "HTTPS on port 443" and "a trusted certificate": builders use
port 8443 with Stackwright's own CA.

## Context

The control plane must tell a build server to start and stop sandboxes, and
the build server must report its health. Build servers can sit behind NAT.
uncloud sends its CLI's calls over SSH and joins machines with a WireGuard
mesh, so that containers on different machines can talk to each other.
Stackwright has no traffic between build servers: every flow is between one
build server and the control plane.

## Decision

- The builder opens one outbound connection to the control plane's public
  URL: a bidirectional gRPC stream over HTTPS on port 443.
- It authenticates with a client certificate that the control plane issues at
  enrolment. The certificate names the build server and its org. It is short
  lived and renewed over the stream.
- The control plane sends start and stop orders down the stream. The builder
  sends capacity, labels, sandbox status and a heartbeat every 15 s.
- The builder needs no open port. SSH is used only to install and upgrade.
- The stream sits behind a Go interface, so a second transport can be added.

## WireGuard

WireGuard is not used now. A hub-and-spoke tunnel would keep control-plane
endpoints off the internet and would give sandboxes a plain route to the
control plane. It costs an open UDP port, key and address allocation that we
would build, and network-level access for a compromised build server. We
need no mesh, because build servers do not talk to each other. WireGuard can
be tried later as a second transport behind the interface.

## Alternatives

- **SSH as the control channel, as in uncloud.** Builders hold no
  credentials, but the control plane must reach every build server, and
  every call needs an SSH session.
- **WireGuard hub-and-spoke.** See above.

## Consequences

- Remote build servers need the control plane on HTTPS with a trusted
  certificate. The built-in builder talks to it inside compose.
- Revoking a build server revokes its certificate and closes its stream.

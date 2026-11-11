# Decision records

Each record states a decision, the alternatives, and why the decision won. Do
not rewrite an accepted record. Write a new one that supersedes it, and add a
line to the old one that points to the new one.

| # | Decision |
| --- | --- |
| [0001](0001-apache-2-licence.md) | Apache-2.0 licence |
| [0002](0002-one-control-plane-binary.md) | One control-plane binary |
| [0003](0003-nats-inside-the-control-plane.md) | NATS JetStream inside the control plane |
| [0004](0004-key-file-with-optional-vault.md) | A key file by default, Vault Transit optional |
| [0005](0005-s3-required-rustfs-in-compose.md) | S3 required, RustFS in compose |
| [0006](0006-agent-in-go-through-openrouter.md) | The agent in Go, models through OpenRouter |
| [0007](0007-sandbox-backends-and-build-servers.md) | Sandbox backends and build servers |
| [0008](0008-build-server-channel.md) | Build servers dial out over gRPC with mTLS |
| [0009](0009-local-accounts-and-oidc.md) | Local accounts and OIDC, claimed with a setup code |
| [0010](0010-sandboxes-reach-s3-by-presigned-url.md) | Sandboxes reach S3 through presigned URLs |
| [0011](0011-agent-session-sandboxes.md) | Each agent session gets a fresh sandbox |
| [0012](0012-two-database-logins.md) | Two database logins |
| [0013](0013-sign-in-providers-and-accounts.md) | Sign-in providers in Settings; accounts by invite only |
| [0014](0014-many-orgs.md) | Many orgs in one instance |
| [0015](0015-invite-links.md) | Invites are links |
| [0016](0016-projects-before-repositories.md) | Projects before repositories |
| [0017](0017-personal-tokens-for-state.md) | Personal tokens for the state backend |
| [0018](0018-docker-sandboxes.md) | Docker sandboxes: our own API client, an engine mirror, open egress |

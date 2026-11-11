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

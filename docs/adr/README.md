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
| [0019](0019-terraform-stacks-files.md) | Terraform Stacks files, run by Stackwright |
| [0020](0020-runs-postgres-queue.md) | Runs: a Postgres queue, uploaded code, joined networks |
| [0021](0021-build-server-enrolment.md) | Build servers: a builder port with its own CA, a binary install, a builder-filled cache |
| [0022](0022-variables-and-cloud-credentials.md) | Variables and cloud credentials: an issuer the server serves, secrets in the claim |
| [0023](0023-github-app.md) | GitHub: one App per instance from a manifest, forks never plan |
| [0024](0024-git-in-sandboxes-and-linked-runs.md) | git in the sandbox image, and linked projects run from their branch |
| [0025](0025-newer-terraform.md) | Newer Terraform, and engine versions from the release lists |
| [0026](0026-applies-start-from-the-plan-tree.md) | Applies start from the plan's working tree |
| [0027](0027-plan-graph.md) | The plan graph: a node for each resource, edges from the configuration, ELK in the browser |
| [0028](0028-live-apply-and-replay.md) | Live apply and replay: the run page splits, one reducer, gaps capped at 2 s |
| [0029](0029-sidebar-navigation.md) | A collapsible sidebar instead of the toolbar and workspace tabs; the plan graph on the run page only |
| [0030](0030-agent-sessions.md) | Agent sessions: the toolbox, the Git proxy and the cap |
| [0031](0031-agent-plans-and-pull-requests.md) | Agent plans are plan-only runs that nothing can apply; pull requests need a person |
| [0032](0032-grounding-and-evals.md) | Grounding from the sandbox: provider schemas, module sources and repository facts; an eval that plans the proposed commit |
| [0033](0033-agent-sandboxes-outlive-a-restart.md) | A restarted server adopts agent sandboxes; each sandbox start has its own job id |
| [0034](0034-editor.md) | The editor edits a session's checkout; saves merge with the agent and never commit; tofu-ls behind a relayed WebSocket |

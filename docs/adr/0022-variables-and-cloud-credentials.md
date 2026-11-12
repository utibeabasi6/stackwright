# 0022 — Variables and cloud credentials: an issuer the server serves, secrets in the claim

**Status:** Accepted, 2026-11-12.

## Context

Phase 7 gives runs variables and short-lived AWS credentials, with no
static keys stored. AWS trades a JWT for credentials only if it can fetch
the issuer's discovery document and keys over public HTTPS, on every
exchange. Sensitive variables and the token must reach the sandbox, which
already gets the job token and the artifact key in its ClaimJob answer.

## Decision

**The server is the issuer.** It serves `/.well-known/openid-configuration`
and `/.well-known/jwks.json` on its main port. The issuer URL is
`STACKWRIGHT_OIDC_ISSUER`, or `STACKWRIGHT_PUBLIC_URL` when that is empty.
The signing key is RSA 2048, made on first start and sealed in Postgres with
the instance data key, like the builder CA (0021). Its `kid` is its JWK
thumbprint (RFC 7638). The JWKS lists every stored key and the newest one
signs, so a later key rotation can add a key first. An instance that AWS
cannot reach publishes the same two files somewhere public, such as an S3
bucket, and sets `STACKWRIGHT_OIDC_ISSUER` to that URL; `curl` on its own
`/.well-known` paths gives the files. No command writes them.

**A token for each job, minted at the claim.** The token lasts 15 minutes.
Its audience is `sts.amazonaws.com`. Its subject is
`org:<org id>:workspace:<workspace id>:phase:<plan|apply>`: ids survive
renames, so a new workspace with an old name never inherits trust. Other
claims name the org, project, workspace, run and job, for audit. A job whose
workspace has a cloud account fails before it is queued when the issuer is
not https, and the run says why.

**Secrets travel in the claim.** The ClaimJob answer carries the effective
variables, with sensitive values opened, and the AWS role, region, token and
STS endpoint. It is built only after the one-time ticket is spent, as the
job token and the artifact key already are. A remote sandbox's server URL
must be https in production; that already holds for the artifact key.

**Variables.** Org, project and workspace scopes; two kinds: input variables
(`TF_VAR_<key>`) and environment variables. For each kind and key, the
workspace's value wins over the project's, which wins over the org's. A
sensitive value is sealed with the org's data key, its plain column is
NULL, and no API answer holds it. It needs 6 characters at least, so masking
it does not mask common text. Environment names that would change the
engine's credentials, backend, proxies or paths are refused: the `TF_`,
`AWS_`, `ARM_`, `GOOGLE_` and `STACKWRIGHT_` prefixes, `HOME`, `PATH`,
`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and `SSL_CERT_FILE`. The executor
also puts its own entries after the variables, so the last one wins.

**Cloud accounts.** An AWS account has a plan role, an apply role and a
region. The roles must differ and be in one account of the `aws` partition.
`GetAwsSetup` writes each role's trust policy: the issuer's audience, and
the exact subject of each workspace that uses the account, for that role's
phase only. It also writes a CloudFormation template that makes the OIDC
provider and both roles, with `ReadOnlyAccess` for plans and
`AdministratorAccess` for applies by default.

**The engine gets the credentials and nothing else does.** The executor
calls `AssumeRoleWithWebIdentity` itself, unsigned, and puts the
credentials only in the engine's environment. They last an hour. The
executor masks the job token, the web identity token, the credentials and
every sensitive value in events, `log.jsonl`, `plan.json` and error text, as
written and as JSON strings. `executor.credentials_issued` names the role
and the access key ID.

## Alternatives

- **Static files only, published by the operator.** Exchanges keep working
  while Stackwright is down, but each install needs a hosting step and a
  new upload after a key change.
- **Secrets as files from the builder.** They would stay on the mTLS stream,
  but the server would build them at dispatch, and the job token and the
  artifact key would still travel in the claim.
- **Keys from `.env`.** Another secret for `make init` to make and the user
  to back up, for a key that the database can hold sealed.

## Consequences

- `plan.bin` and the state still hold sensitive values, because the engine
  needs them as they are. Both are sealed at rest, and the API never serves
  `plan.bin`.
- Adding a workspace to an account changes its trust policies, so the user
  deploys the template again.
- A job longer than an hour cannot renew its AWS credentials.
- Only AWS works. The schema allows other providers later.

# Running Stackwright

This doc is for the team that runs a Stackwright install with
`compose.yaml`. The decisions behind it are in
[adr/0046](adr/0046-running-in-production.md).

## What holds the data

| Part | Holds | Without it |
| --- | --- | --- |
| Postgres | Every row: users, runs, the audit log, wrapped data keys | Nothing works |
| The bucket (`STACKWRIGHT_S3_BUCKET`, default `stackwright`) | State versions, plans, logs and uploads, all encrypted | State and run history are gone |
| `.env` | `STACKWRIGHT_KEK` and the other secrets | The bucket and every secret column cannot be opened |

Back up all three. Keep the copy of `.env` apart from the other two: with
the KEK, a copy of the database and the bucket can be read.

## TLS

The server serves plain HTTP on port 8080. Put a TLS proxy in front of
it. With Caddy:

```
stackwright.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Then set these in `.env`:

- `STACKWRIGHT_PUBLIC_URL=https://stackwright.example.com`. An https URL
  makes the session cookies `Secure`.
- `STACKWRIGHT_TRUSTED_PROXIES` to the proxy's address, such as
  `127.0.0.1` or `172.16.0.0/12`. The server then takes the client's
  address from `X-Forwarded-For` for its rate limits. It takes the
  rightmost address that is not a trusted proxy. Do not trust an address
  that clients can reach directly.

Port 8443 is for build servers. It uses TLS with client certificates from
the builder CA, so a proxy in front of it must pass TLS through. Publish
it directly, and set `STACKWRIGHT_BUILDER_URL` to the address that build
servers use.

## Backup

```bash
docker compose exec -T postgres pg_dump -U stackwright -Fc stackwright > stackwright.dump
```

Copy the bucket with any S3 tool, for example
`mc mirror local/stackwright backup/stackwright`. Copy `.env`.

A dump taken while runs write state can hold a state version whose
object was written after the bucket copy started. Copy the bucket after
the dump finishes, so every row in the dump has its object.

## Restore

On the new host:

1. Put the `.env` from the backup in place. Use the same release as the
   backup.
2. Run the migrations on the empty database:
   `docker compose run --rm migrate`. This makes the `stackwright_app`
   role, which belongs to the Postgres cluster and is not in the dump.
3. Restore the dump over it:

   ```bash
   docker compose exec -T postgres pg_restore -U stackwright --clean --if-exists --exit-on-error -d stackwright < stackwright.dump
   ```

4. Copy the bucket back.
5. Start the rest: `docker compose up -d --wait`.

`TestRestoreFromABackup` in `internal/server/backup_test.go` does steps 2
to 4 on a new database in the same Postgres. It then reads the same
state through a server on the copies. It does not use a second host.

## Upgrades

1. Back up.
2. Get the new release's `compose.yaml` and images.
3. Run the migrations: `docker compose run --rm migrate`.
4. Start the new server: `docker compose up -d --wait`.

Runs that wait for approval, and runs in progress on a build server,
continue after the upgrade. Server N accepts build servers and executors
of N-1, so remote build servers can be upgraded after the server.

`TestUpgradeWhileARunWaits` in `test/upgrade/` checks this. It builds the
last release tag with Compose, makes a run that waits for approval,
upgrades only the server to the working tree, and approves the run. The
run must then apply on the old builder. It needs Docker and about 15
minutes:

```bash
STACKWRIGHT_TEST_UPGRADE=1 go test -timeout 60m ./test/upgrade/
```

Set `STACKWRIGHT_UPGRADE_FROM` to test from a commit instead of the last
tag.

## Rotating the KEK

The KEK wraps one data key for each org and one for the instance. A
rotation wraps the data keys again. It does not touch the data that they
encrypt, so it is fast and the server keeps running.

1. Make a new KEK: `openssl rand -base64 32`.
2. In `.env`, move the current `STACKWRIGHT_KEK` to
   `STACKWRIGHT_KEK_PREVIOUS`, and set `STACKWRIGHT_KEK` to the new one.
3. Restart the server: `docker compose up -d --wait`. It opens data keys
   that either KEK wrapped, and wraps new data keys with the new KEK.
4. Wrap every data key with the new KEK:
   `docker compose run --rm migrate rotate-kek`. It works in one
   transaction and prints how many keys it changed.
5. Remove `STACKWRIGHT_KEK_PREVIOUS` from `.env`, and restart again.
6. Back up the new `.env`. Backups of the database from before the
   rotation still need the old KEK.

If `rotate-kek` says that a data key was wrapped by another KEK, that key
is from neither KEK in `.env`. Nothing changed.

## Rotating the built-in builder's token

1. In `.env`, move `STACKWRIGHT_BUILDER_TOKEN` to
   `STACKWRIGHT_BUILDER_TOKEN_PREVIOUS`, and set a new
   `STACKWRIGHT_BUILDER_TOKEN`.
2. `docker compose up -d --wait`. The builder enrols with the new token.
3. Remove `STACKWRIGHT_BUILDER_TOKEN_PREVIOUS`, and restart again.

A remote build server's join token works once. Its certificate lasts a
day and renews on its stream, so there is nothing to rotate. Remove a
build server in the console to revoke it.

The database passwords and the S3 keys have no rotation command. Change
them in Postgres or the object store and in `.env`, then restart.

## Rate limits

The server limits requests with a token bucket for each caller:

| Route | Key | Rate | Burst |
| --- | --- | --- | --- |
| Sign-in, invites, setup, `/auth/` (OIDC, GitHub, SAML), the device flow | Client IP | 5 a second | 30 |
| The API | Session or token, else client IP | 50 a second | 300 |
| SCIM | Client IP | 20 a second | 200 |

A caller over the limit gets 429, or `resource_exhausted` over Connect,
with `Retry-After`. Jobs, build servers, the state backend and webhooks
are not limited: each has its own token. The counts live in the server
process.

## What sandboxes can reach

A sandbox's network is internal. Its only way out is the egress proxy
that its builder starts. The proxy allows public addresses and the
server URL of the sandbox's order. It refuses private, loopback,
link-local, shared and multicast addresses, and logs each refusal.

- A provider that does not speak HTTP or HTTPS, such as a database
  provider or an SSH provisioner, cannot reach its target.
- A provider that must reach a private API, such as a Vault or a
  Kubernetes API on a private network, is refused too. To allow one, set
  `STACKWRIGHT_EGRESS_ALLOW` on the builder to a comma-separated list of
  `host:port` pairs. Every sandbox on that builder can then reach them.
- With `STACKWRIGHT_S3_PUBLIC_URL` empty, sandboxes reach object storage
  through the server, under `/s3/`. Set it for an object store that
  sandboxes can reach directly, such as AWS S3.

See [threat-model.md](threat-model.md) for what this protects against.

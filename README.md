# Stackwright

Stackwright is an open-source, self-hosted workspace for infrastructure code.
An AI agent writes OpenTofu and Terraform. Sandboxes on your own machines run
the plans. Every change shows as a live graph.

**Status:** early. The build follows [docs/BUILD.md](docs/BUILD.md) one phase at
a time; [docs/status.md](docs/status.md) says what works today.

## Run it

You need Docker with Compose v2.

```bash
make init
docker compose up -d
```

`make init` writes `.env` with new random secrets. Keep it out of git and back
it up: it holds the keys to your data. After an upgrade, run `make init`
again: it adds any secret that the new version needs and keeps the others.

Compose also starts the built-in build server, `builder`. It starts a sandbox
for each plan and apply. It is the only container with the Docker socket,
which gives root on the host. Then open http://127.0.0.1:8080 and
enter the setup code from this command:

```bash
docker compose logs stackwright | grep setup_code
```

### Your own build servers

Build servers are optional. To run jobs on more machines, enrol them from
the Build servers page, over SSH or with a command you run on the machine.
A build server needs Linux, Docker and systemd. It dials out to the
Stackwright host, so set these in `.env` to addresses that it can reach:

| Setting | Holds |
| --- | --- |
| `STACKWRIGHT_BUILDER_URL` | The builder port, such as `https://stackwright.example.com:8443`. It serves TLS from Stackwright's own CA, so a proxy in front must pass TLS through. |
| `STACKWRIGHT_SANDBOX_URL` | The server, as the build server's sandboxes reach it. |
| `STACKWRIGHT_S3_PUBLIC_URL` | The S3 endpoint, as those sandboxes reach it. |
| `STACKWRIGHT_EXECUTOR_IMAGE` | The sandbox image that build servers pull. |

Compose publishes the builder port and RustFS on 127.0.0.1 only. Publish
them beyond this host for build servers on other machines.

To reset a password when nobody can sign in:

```bash
docker compose run --rm migrate user reset-password -email you@example.com
```

## Develop

You also need Go 1.26 and Node 22.

```bash
make up      # the stack, with Postgres, NATS and RustFS on local ports
make dev     # the web app with live reload on :5173
make test
make lint
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a pull request.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

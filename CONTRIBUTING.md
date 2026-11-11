# Contributing

Thank you for helping. Read [docs/README.md](docs/README.md) first: it lists the
docs in the order to read them.

## Before you write code

- Large changes follow a phase in [docs/BUILD.md](docs/BUILD.md). Open an issue
  first if your change is not in a phase.
- A decision that is expensive to undo needs a record in
  [docs/adr/](docs/adr/). Do not rewrite an accepted record; supersede it.

## Pull requests

- `make lint` and `make test` pass. `make test` needs `make init` and `make up`
  once.
- Update the docs in the same pull request that makes them wrong.
- New dependencies are pinned to an exact version that is at least 14 days
  old, with a licence that `make licenses` allows.
- Keep comments for what the code cannot say. A function used once is
  inlined.

## Sign your commits

Every commit needs a `Signed-off-by` line. It certifies that you wrote the
change or have the right to submit it, under the
[Developer Certificate of Origin](https://developercertificate.org/).
`git commit -s` adds it.

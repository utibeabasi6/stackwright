# 0036 — The importer, and runs on the code's own backend

**Status:** Accepted, 2026-11-12. Changes phase 15 in BUILD.md: no state
copy and no migration pull request.

## Context

Phase 15 imports an existing repository. BUILD.md planned to copy state
into Stackwright and open a pull request that switches backends. The user
decided against both: the repository must keep working without
Stackwright, with people running the engine from their machines and their
own CI against their own backend. Two live copies of the state would
drift the moment someone applies outside Stackwright.

## Decision

**One source of truth: the code's backend.** A project's state is either
Stackwright's (as before) or the code's. For the code's backend, runs
write no override file and initialize with the backend block as written,
with the workspace's variables and cloud credentials, so an S3 backend
gets the role's AWS keys. Locking is the backend's own, so a run from a
laptop and a run in Stackwright cannot apply at once. A CLI workspace is
chosen with `TF_WORKSPACE`. Nothing is copied and nothing in the
repository changes.

**State committed to Git is plan-only.** A root module whose state is a
committed `.tfstate` file (the local backend) plans from that file. Stackwright cannot write it back
without committing to the repository, so the database refuses an apply
for such a project, as it does for agent plans.

**The scan reads the repository archive in the server.** It parses `.tf`
and `terragrunt.hcl` files and runs no code. It finds root modules (a
folder with `.tf` files that no other folder calls as a local module),
shared modules, CLI workspaces (`terraform.tfstate.d/<name>/`), committed
state files, the backend type of each root, and Terragrunt units, which
are listed and wait for phase 18.

**An import creates projects and workspaces, then plans each one.** A
project is found again by repository and path, a workspace by name, so a
second import creates nothing new. Each workspace then gets a plan-only
run; the import's page shows those runs, so its progress survives a
reload, and "no changes" on every one is the check that the import read
the right state.

## Alternatives

- **Copy the state into Stackwright, and a pull request that switches the
  backend.** One source of truth only if everyone stops using the old
  backend; the repository would no longer work alone.
- **Keep both copies in sync.** Two writers and no shared lock; a race
  away from lost state.
- **Copy committed state into Stackwright.** It diverges from the file in
  Git with the next commit.

## Consequences

- For a project on the code's backend, Stackwright has no state history,
  rollback or lock of its own; its state page says where the state lives.
  The agent's `state_lookup` still works, as it reads the plan.
- Stackwright needs the backend's credentials. A backend that the
  workspace's cloud credentials and variables cannot reach fails at init.
- A committed-state project can never apply in Stackwright until its code
  moves to a remote backend.

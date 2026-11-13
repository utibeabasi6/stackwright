# 0035 — Project labels instead of groups

**Status:** Accepted, 2026-11-12.

## Context

A repository often holds one root module per environment or component,
such as `envs/dev` and `envs/prod`. People want to see those projects
together. The user first proposed project groups.

## Decision

**Projects carry labels**, `key=value` pairs with the same rules as build
server labels: `env=prod`, `app=payments`, `team=platform`. The importer
sets `repo` and `path`, and proposes `env` from folder names for the
person to confirm.

**Groups are views, not stored structure.** The project list groups by
one or more label keys, in the order the person picks: by `env`, or by
`env` then `app` for nested groups. A `path` value splits on `/` into a
tree like the repository's. The browser remembers the choice. A project
without a key shows under "No <key>".

**Settings and policy do not hang off groups.** Later phases may target a
selector, such as variables or approval rules for `env=prod`; that is a
decision of its own.

## Alternatives

- **Project groups.** Familiar, but one axis and one parent for each
  project, and settings inherited down a tree are hard to reason about.
- **Environments as an object.** Strong for policy and ordered rollouts,
  but only one axis; it can sit on top of labels later.
- **The repository as the grouping.** Free, but only for linked projects,
  and it says nothing about environments.

## Consequences

- Labels can be inconsistent (`env=prod` and `environment=production`);
  the label field suggests keys already in use.
- One value for each key, so a project shows once in each tree.

# 0006 — The agent in Go, models through OpenRouter

**Status:** Accepted, 2026-11-11.

## Context

The first build ran the agent loop in a TypeScript service (its ADR 0017)
and called one model provider. That gave the repo two backend languages, two
lint and test setups, and a network seam that needed a service token.
Self-hosted users also want to choose their model and pay for it with their
own account.

## Decision

- The agent loop is a Go package inside `stackwright server`
  ([0002](0002-one-control-plane-binary.md)). The tools still run in a
  toolbox sandbox, not in the server.
- Models are called through OpenRouter, with the org's own API key. The key
  is sealed like every other secret ([0004](0004-key-file-with-optional-vault.md)).
- The model client is one Go interface with one implementation. A second
  provider can be added behind it later.
- Each org sets two models: one for agent turns and one for short tasks
  (titles, summaries). The default is Claude Opus 5.5 for turns and Claude
  Haiku 5.5 for short tasks.
- Each org has a monthly spending cap. At the cap, new agent turns stop.
  Plans and applies keep running.
- Without a key, the agent is off. Everything else works.

## Not verified yet

These are checked in phase 11 against OpenRouter's documentation and a real
call, with the user's approval for the spend:

- How OpenRouter reports the cost of each call, which the cap needs.
- How prompt caching is turned on for Claude models through OpenRouter.
- Whether to call the HTTP API directly or use a published Go client.

## Alternatives

- **Keep the TypeScript service.** The first build's code works, but it keeps
  two languages and the seam.
- **Call each model provider directly.** It gives full control of each API,
  but every provider is its own client, key and bill.

## Consequences

- The first build's TypeScript agent and its scripted-model tests are ported
  to Go, not copied.
- Eval runs (phase 13) spend the user's money. Each run needs their approval.

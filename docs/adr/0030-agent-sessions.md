# 0030 — Agent sessions: the toolbox, the Git proxy and the cap

**Status:** Accepted, 2026-11-12. Builds on [0006](0006-agent-in-go-through-openrouter.md)
and [0011](0011-agent-session-sandboxes.md), and answers what 0006 left to
check.

## Context

Phase 11 adds the agent. 0006 and 0011 decided the loop runs in the server,
each session gets its own sandbox with a clone, and nothing reaches GitHub
without a person's approval. They left open how the server talks to the
sandbox, how pushes are held to the approval, how cost is read, and where
the agent shows in the interface.

## Decision

**OpenRouter, called over HTTP.** The model client is one Go interface with
an OpenRouter implementation that calls `/api/v1/chat/completions` with
tools; no Go client library. Each response's `usage.cost` is the call's
cost in credits (US dollars), with no request flag needed. A top-level
`cache_control: {"type": "ephemeral"}` turns on Claude's automatic prompt
caching, and `usage.prompt_tokens_details.cached_tokens` shows the hits.
Both were read from OpenRouter's documentation on 2026-11-12. The default
models are `anthropic/claude-opus-5.5` for turns and
`anthropic/claude-haiku-5.5` for titles.

**The cap counts what OpenRouter charged.** Each model call's cost is stored
with its step. A turn sums the org's costs for the calendar month (UTC)
before each model call; at or over the cap, the turn stops with a message
that names the cap and the spend.

**The sandbox dials the server.** An agent sandbox is the executor image
started as `stackwright-executor toolbox` on a build server slot, with a
session token; there is no separate toolbox binary. The
toolbox connects out to the server on a bidirectional stream and waits for
work: the server sends a tool call or a commit order, and the toolbox
answers. The server never connects into a sandbox, so this works with build
servers behind NAT. The toolbox refuses state files (`*.tfstate`,
`*.tfstate.backup`, `.terraform/`) and `.env` files in every tool, and
refuses paths outside the checkout; the server checks the same before it
sends a call.

**The Git proxy holds pushes to approvals.** The sandbox clones and pushes
through `/git/<session>/` on the server, with the session ticket as its
password. The proxy forwards to GitHub with the installation token, which
never enters the sandbox. A fetch is allowed. A push is allowed only when
every ref it updates is the session's branch, `agent/<session slug>`, and
the session has an approved proposal that no push has used yet; the push
uses it. So no tool, prompt or bug can push without a person's stored
approval.

**A commit only follows an approval.** The agent's `propose_commit` tool
stores the files, the diff and a message, and stops the turn. "Commit and
push" records the approver, then orders the toolbox to commit as that
person and push. "Discard" orders it to reset the checkout.

**The agent is a panel.** A panel on the right of every workspace page,
shown or hidden with ⌥⌘0 and remembered per browser, holds the workspace's
sessions and the open one. With no key, it says so and links to the
Agent settings; nothing else depends on it.

**Linked projects only.** A session needs a repository that a linked GitHub
installation covers. Other projects show the panel with a link to the
GitHub settings.

**Agent sandboxes have open egress for now**, as run sandboxes do
([0018](0018-docker-sandboxes.md)). The no-key, no-token rules hold. Limiting
egress to the Git proxy and the registries is a known gap.

## Alternatives

- **The server execs into the sandbox.** Simpler on the built-in builder,
  but a remote build server would need an inbound channel to each sandbox.
- **Push from the toolbox with a short-lived token.** Less proxy code, but a
  token in the sandbox can push anywhere until it expires.
- **An Agent page in the workspace's sidebar.** One more page, but the
  conversation would not sit beside the plan or run it talks about.
- **An egress allowlist now.** Closes the gap, but needs an egress proxy
  and a list of every provider's download host.

## Consequences

- An approval is spent by one push. A push that the proxy or GitHub refuses
  at the HTTP level leaves it unspent, so "Commit and push" can be tried
  again. A push that GitHub takes but whose ref update fails, such as a
  non-fast-forward, spends it.
- The cap is checked between model calls, so one call can pass it by its
  own cost.
- A real model's answers vary, so the checks against it assert what must
  hold whatever it says: refused files stay refused, and nothing is pushed
  without an approval.

# 0034 — The editor

**Status:** Accepted, 2026-11-12. Builds on [0030](0030-agent-sessions.md)
and [0033](0033-agent-sandboxes-outlive-a-restart.md).

## Context

Phase 14 lets people edit files by hand, with language support, on the
agent's branch. BUILD.md said a save commits as the person. The user
chose otherwise: a save only writes the file, and the person tells the
agent to commit, through the proposal that a person approves.

## Decision

**A session is where people edit.** The Editor page edits the checkout of
one agent session, in its sandbox, on its branch. With no session, "Edit
files" starts a quiet session: a sandbox and a branch, no message and no
model call. The agent can work in the same session later.

**A save writes the checkout and nothing else.** No commit, no push. The
changes join the agent's next proposal, which a person approves as
before; the Git proxy's rule is unchanged.

**A save never loses either change.** The toolbox runs one order at a
time, so a save lands between two tool calls, never during one. The
editor sends the file as it opened it and the new text; when the file
changed since, the toolbox merges the three line by line with
`git merge-file`. A conflict writes nothing and returns the file as it is
now. The agent's next model call gets a note that the person saved the
file, so the model reads it again. The note waits until the turn's open
tool calls have their results.

**`tofu-ls` runs in the sandbox, behind a WebSocket.** The browser opens
`/lsp/<session>` on the server with its session cookie; the server checks
the Origin and that the person may write to the workspace. The server
relays JSON-RPC frames over the toolbox's stream, so it still never
connects into a sandbox. The toolbox starts one `tofu-ls` for each
WebSocket and maps `file:///repo/` to the checkout. `tofu-ls` serves both
OpenTofu and Terraform projects. The executor image holds `tofu-ls`
0.5.3, checked against its release checksums.

**Our own LSP client.** Monaco's completion, hover, diagnostics and
formatting providers speak to `tofu-ls` through a small client: only the
requests the editor uses. Format on save asks `tofu-ls` to format the file
first.

**Keyboard only.** The file list is a tree and the open files are tabs,
both with arrow keys. F6 moves focus between the tree, the tabs and the
text. In the text, Ctrl+M makes Tab move focus, as Monaco documents.

## Alternatives

- **A save commits and pushes as the person** (BUILD.md). Each save would
  be its own commit, and the proxy would need a second kind of approval.
- **A browser-side merge.** The browser cannot see the checkout as the
  agent leaves it, between tool calls.
- **monaco-languageclient.** Every LSP feature, but it pulls in the VS Code
  API packages.
- **Monaco's own LSP client.** Monaco 0.57 ships `monaco-lsp-client`, found
  after this choice; nobody has tried it. Ours is about 200 lines and
  covers what the editor uses.
- **terraform-ls for Terraform projects.** Closer to Terraform, but a
  second binary to pin and update.

## Consequences

- Saved changes are not on GitHub until a person approves a proposal.
- A sandbox that is removed loses unsaved editor tabs, as before; saved
  ones stay in its checkout while it is dirty.
- Monaco is loaded only by the Editor page, so the rest of the app does
  not grow: the main chunk is the same size as before. The build also
  emits Monaco's JSON, HTML, CSS and TypeScript workers, which are never
  fetched.
- Monaco uses dompurify, "MPL-2.0 OR Apache-2.0"; Stackwright takes it
  under Apache-2.0.

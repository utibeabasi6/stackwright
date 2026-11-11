# Stackwright — design system

Stackwright is an open-source, self-hosted workspace for infrastructure code. An AI agent writes OpenTofu and Terraform. Sandboxes on your own machines run the plans. Every change shows as a live graph. The interface looks and behaves like a native desktop tool: the platform's own fonts, a toolbar, tabs for open workspaces, and an inspector on the right. It follows the system's light or dark setting.

## Voice

- Do: write like a careful senior engineer. Short sentences, exact numbers, resource addresses in mono (`aws_nat_gateway.main`).
- Do: say what will change before it changes ("3 to add, 1 to destroy").
- Don't: hype, exclamation marks, emoji, or soften a destroy. A destroy is always named as a destroy.
- The agent speaks in first person and plain verbs: "I added a NAT gateway and planned it."
- Setup text names the exact command and the exact file. "Run `docker compose up -d`", not "start the stack".
- Sentence case everywhere: titles, buttons, menu items and labels. No all-caps labels.

## Visual foundations

**Surfaces.** Content sits on `window`. The toolbar and bottom bars use `chrome`. Inactive workspace tabs sit on `tabstrip`, and the active tab takes `window`. The inspector uses `inspector`. The plan graph and topology sit on `canvas` with a 16px dot grid. Panes are separated by 1px `hairline` borders. Rows inside a pane are separated by `separator`.

**Depth.** Flat. Only menus, popovers and the commit sheet float, with one soft shadow (`0 8px 24px` at 14% black). Cards have a `hairline` border and no shadow. Never put a card inside a card.

**Accent.** `accent` is the one action colour: the default button in a view, the count on "Waiting on you", the selected segment's marker. A view has at most one accent button. Text on an accent fill is `on-accent`. Links and the focus ring use `accent-ink`.

**Plan semantics.** Four colours describe a plan: `create`, `update`, `replace`, `destroy`. Each always travels with its glyph (`+`, `~`, `-/+`, `-`), so the meaning survives for colour-blind readers and in monochrome screenshots. Nodes and rows that change get a wash: `create-wash` or `update-wash`. `destroy` doubles as the error colour.

**Status.** Outside a plan, three of those colours mark health, always with a glyph and a word:

| State | Token | Example |
| --- | --- | --- |
| Good | `create` with `✓` | `✓ Online`, `✓ Applied` |
| Needs attention | `update` with `~` | `~ Draining`, `~ 2 drifted` |
| Broken | `destroy` with `!` | `! Unreachable`, `! 1 high finding` |
| Idle or unknown | `ink-muted`, no glyph | `Not imported`, `Never connected` |

**Type.** The platform's UI font (`sans`) and monospaced font (`mono`). No web font is loaded. Body text is 13px. Page titles use `large-title`; views and cards use `title`. Group labels in menus use `label`, in sentence case. Every resource address, command and log line is `code` or `log`.

**Space and shape.** A 4px grid: `space-1` to `space-6`. Controls are 32px high (`control-height`) with `radius-control`. Cards, menus and sheets use `radius-card`. Count badges and status pills use `radius-pill`.

**Controls.** Buttons have a `hairline` border on `window`, or an `accent` fill for the one default action. View switching uses a segmented control: the selected segment is a raised `window` chip on a `tabstrip` track. Fields are 30px with a `hairline` border.

**Focus.** Keyboard focus is a 2px `accent-ink` ring, 2px outside the control.

**Size.** Controls are 32px, which meets the WCAG 2.2 target size of 24px. Stackwright is a desktop tool for a pointer and a keyboard. It is not built for touch.

## Terminology

Stackwright supports plain `.tf` folders, Terraform and OpenTofu workspaces and Terragrunt layouts, so the UI uses two neutral words:

- **Project:** a folder of IaC code, one root module (`network/`, `legacy/bastion/`).
- **Workspace:** one state of that project. A Terraform or OpenTofu workspace (`dev`, `staging`, `prod`), or the single `default` state of a plain folder.

Show the engine's own word beside the neutral one when they differ. Never call a plain folder a stack.

Where runs happen:

- **Build server:** a machine that starts sandboxes. "Built-in" is the one on the Stackwright host. Never "runner", "worker" or "node" in the UI.
- **Sandbox:** one isolated environment for one job or one agent session. The **sandbox provider** says how it is made ("Docker" today).
- **Enrol:** to add a build server. The opposite is **Remove**. A server that finishes its jobs and takes no new ones is **Draining**.

The agent's model comes from a **model provider** (OpenRouter). Show the model by its plain name ("Claude Opus 5.5").

The agent **proposes** commits. A person **commits and pushes**. A plan made from uncommitted changes is **speculative** and cannot be applied.

**Security severity.** Findings use `destroy` for Critical and High, `update` for Medium, `ink-muted` for Low and Info, always with the word written out.

## Layout

There is no sidebar. Two bars and one inspector hold the navigation.

**The toolbar** (`chrome`, one row): the app name and org on the left. In a workspace, a segmented control in the centre switches the view: Overview, Plan, Topology, Security, Editor, Log. On the right: Projects, "Waiting on you" with its count, search (⌘K), Build servers, and Settings. The app name also goes to Projects.

**The workspace tab strip** (`tabstrip`, under the toolbar): one tab for each open workspace, named "project › workspace". A tab carries one status mark when something needs the user: an `accent` dot for agent changes, `~` for drift, `!` for a failure or a High finding. "+" opens a workspace by name; ⌘T does the same. ⌘W closes a tab. Tabs stay open between visits.

**Organisation pages** (Projects, Waiting on you, Build servers, Settings) open in the content area with no tab selected. Their content column is at most 1120px, with `space-6` margins.

**A workspace** fills the area under the tab strip with its view. A bar above the view names what it shows ("Plan #142 · Speculative · +3 ~1 −0"). A bar below the view says what happens next, in `chrome`.

**The inspector** (`inspector`, 360px, on the right) has two tabs: Agent and Resource. It opens by default on the Plan view and remembers whether the user hid it. ⌥⌘0 shows or hides it. The Agent tab names the branch and the sandbox it works in. A proposed commit shows as a card titled "Commit these changes?" with "Discard", "Show diff" and the accent "Commit and push".

**Editor.** People can always edit files by hand. The Editor view adds a file list on its left. Syntax uses `syntax-keyword`, `syntax-string`, `syntax-number`, `ink` for attributes and `ink-muted` for comments.

**Commands.** A command the user copies sits in a `chrome` block with a `hairline` border and `radius-control`, in `code` style, with a Copy button. Secrets in a command are never shown in full: show the first 8 characters and a Reveal button.

## Iconography

No icon set yet. Use 16px single-stroke outline icons at 1.5px stroke in `ink-muted`, `ink` when active. Text labels come first; an icon alone needs an accessible name.

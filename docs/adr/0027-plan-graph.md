# 0027 — The plan graph

**Status:** Accepted, 2026-11-12.

## Context

Phase 9 shows every plan as a graph. The plan's JSON (`show -json`) lists
each instance's change and the configuration's references, but it has no
file positions and leaves out `locals`. A plan can have hundreds of
resources, and a layout that moves on every plan is hard to read.

## Decision

**One node for each resource, not each instance.** The node id is the
resource's address without instance keys: `module.vpc.aws_subnet.private`.
A node carries its most significant action (delete, then replace, update,
create, read, no-op) and a count for each action. The side panel lists each
instance with its attribute diff. Modules are groups, with ids such as
`module.vpc`.

**Edges come from the configuration.** An edge runs from the thing referred
to, to the resource that refers to it. A reference to a module output
follows the output's own references into the module. A `var` inside a
module follows the module call's argument in the parent. A `local` follows
the local's references, which the executor reads from the HCL, because the
plan's JSON leaves locals out. `depends_on` edges are marked explicit. A
reference or `depends_on` to a whole module joins the module's group.

**The executor maps addresses to files.** After a plan, it parses the root
module and every module in `.terraform/modules/modules.json` with the HCL
parser. It uploads `sources.json`: each module's resources, module calls
and locals, with file and line. A parse error does not fail the plan; the
map is left without that file. Files are relative to the repository root.
A linked project links to GitHub at the run's commit. An upload, or a
module downloaded during `init`, shows the file and line as text.

**The server builds the graph on request.** `GetPlanGraph` reads the plan
job's `plan.json` and `sources.json`, which stay sealed in the object
store, and builds the graph in `internal/graph`. Nothing about the plan is
stored in Postgres, so no plan value lands outside the sealed artifacts.
`GetPlanNode` returns one node's instances with their diffs. Sensitive
values show as "(sensitive)", unknown ones as "(known after apply)".

**ELK lays out the graph in the browser.** elkjs is EPL-2.0 (or
GPL-3.0-or-later); the web licence check allows EPL-2.0 for elkjs alone.
React Flow draws it.

**Layouts are kept per workspace.** People cannot drag nodes. The first
layout of a workspace is stored as a position for each node id. A later
plan keeps known nodes where they were and places new ones where ELK puts
them, moved until they overlap nothing. A writer's browser saves the
result; a viewer's does not. "Re-layout" starts again from ELK.

**Big plans open collapsed.** Over 300 nodes, each top-level module shows
as one node with its counts, and opens on Enter or a click.

## Alternatives

- **dagre (MIT) or a layout of our own.** No licence exception, but dagre
  handles nested groups poorly, and our own layout is more code to keep.
- **One node for each instance.** Exact, but a `for_each` over 50 items
  makes 50 nodes, and edges between instances can only be guessed.
- **Parse the source on the server.** Modules that `init` downloads are not
  in the source archive, so their resources would have no links.
- **Store the graph in Postgres.** Quicker to read, but the diffs hold plan
  values, which today live only in sealed artifacts.
- **Dragging, with positions saved.** More state, and a race when two
  people move nodes.

## Consequences

- A plan made before this change has no `sources.json`. Its graph has no
  file links, and edges through locals are missing.
- `.tf.json` files are not parsed, so their resources have no links.
- Each `GetPlanGraph` downloads and opens `plan.json`. A cache can come
  later if that is slow.
- The web bundle carries elkjs, about 1.5 MB before compression.

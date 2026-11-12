# 0029 — A sidebar instead of the toolbar and workspace tabs

**Status:** Accepted, 2026-11-12. Supersedes rule 1 and rule 3 of the
interface rules in [BUILD.md](../BUILD.md) ("no sidebar", and workspace
views as tabs), the toolbar and workspace tab strip in the design system's
layout, and the Plan tab in [0028](0028-live-apply-and-replay.md).

## Context

The shell had a toolbar with the org, a view switcher, the main links,
search and the user, and under it a strip with one tab for each open
workspace. A workspace page then had a second row of tabs for its
sections, and "Runs" was in both rows. A plan graph could be opened three
ways: the Plan tab, the run page, and a link on the run page. Leaving a
plan or a run page meant finding the way back by hand.

## Decision

**A sidebar on the left holds the navigation.** At the org level: the app
name, the org switcher, Projects, Waiting on you (with its count), Build
servers and Settings. Inside a workspace it changes: "‹ All projects", the
project and the workspace's name, and the workspace's pages: Overview,
Runs, State, Variables, Cloud credentials and Settings. Waiting on you
stays. The theme switch ("Same as the system", Light, Dark), the user and
Sign out sit at the bottom. The theme is kept per browser and applied
before the first paint.

**Each workspace page has its own URL**, such as
`/acme/projects/network/prod/variables`. The pages have no tabs.

**Breadcrumbs lead back.** Inside a workspace, the top bar shows the path,
such as `Projects › network › prod › Runs › Run #4`, each part but the
last a link.

**The plan graph is on the run page only.** A run's page shows its graph
beside the timeline. "Full screen" hides the timeline, and Escape brings
it back. The Plan tab and the run page's link to it are gone.

**The sidebar collapses.** A button in the top bar, or ⌘\ (Ctrl+\ off a
Mac), hides or shows it. Each browser remembers the choice. The top bar
holds that button, the breadcrumbs and search (⌘K).

## Alternatives

- **One row of tabs under the workspace's name.** No repetition, but a
  workspace page would still carry navigation that the sidebar could hold.
- **A project tree in the sidebar.** One click to any workspace, but a long
  list in an org with many projects, and it repeats the Projects page.
- **Keep the Plan tab as a view of the newest plan.** Quick, but a third
  way to reach a graph that a run already shows.

## Consequences

- The status marks that a tab carried (agent changes, drift, a failure)
  need a new home; "Waiting on you" holds them for now.
- Old links with `?tab=` no longer pick a section; they open the Overview.
- The `tab-height` token is gone, and `sidebar-width` is new.

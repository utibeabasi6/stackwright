# 0028 — Live apply and replay

**Status:** Accepted, 2026-11-12.

## Context

Phase 10 shows an apply on the plan graph as it happens, and replays any
finished run. The run's events already hold each engine line with its
instance address, and `WatchRun` already resumes after a sequence number.

## Decision

**The run page splits.** Once a run has a plan, its page shows the timeline
on the left and the graph on the right. The graph follows the apply live.
When the run has finished, the same graph replays it. The Plan tab stays a
still view of the newest plan.

**One reducer for live and replay.** A pure function takes the state and
one run event, and returns the next state. Live folds events as they
arrive; replay folds the events up to a point in time. The latest
`run.dispatched` event for an apply starts the apply and sets every
changing node to waiting. `apply_start` makes an instance in progress,
`apply_complete` done, and `apply_errored` failed. An error diagnostic with
an address marks its node failed and holds the diagnostic for the
inspector. A node is failed if any instance failed, in progress if any
instance is, and done when all its changing instances are done. The
reducer skips an event at or below the last sequence it has seen, so a
resumed stream cannot apply an event twice.

**Node ids are worked out in the browser.** A TypeScript copy of the Go
function strips instance keys. The Go and the TypeScript tests read the
same file of cases, so the two cannot drift. Old runs replay with no
schema change.

**Replay keeps real gaps, capped at 2 s.** Each gap between events is its
real length, but no more than 2 s, and the speed (1x, 2x, 4x, 10x) divides
it. A long create does not stall a replay, and slow resources still look
slow. The scrubber runs over this compressed time.

## Alternatives

- **The Plan tab follows the newest run live.** One view, but the timeline
  and the graph would be on different pages during an apply.
- **A fixed step for each event.** Even pacing, but it hides which
  resources were slow.
- **Real time exactly.** Faithful, but a replay of a 10-minute apply takes
  minutes even at 10x.
- **A node id on each stored event.** One implementation, but events stored
  before the change would need a backfill.

## Consequences

- An apply that a build server ran twice, after a lost sandbox, shows only
  the latest attempt; the timeline still lists both.
- Nodes that never started because the apply failed first stay waiting.

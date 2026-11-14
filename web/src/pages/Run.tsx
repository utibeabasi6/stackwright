import { useEffect, useMemo, useRef, useState } from "react";
import { Action } from "../Action";
import { useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { graphApi, message, runApi } from "../api";
import { AskAgent } from "../agent/AgentPanel";
import { RunFindings } from "./Findings";
import { Divider, paneWidth, useSavedWidth } from "../Divider";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { RunStatus, type Run as RunType, type RunEvent } from "../gen/stackwright/v1/runs_pb";
import { useShell } from "../me";
import { emptyState, statuses, step, timeline } from "../plan/apply";
import { Graph } from "../plan/Graph";
import { Select } from "../Select";
import { useLoad } from "../useLoad";
import { changes, finished, line as describe, origin, runTitle, statusOf } from "../runs";

function useRun(org: string, project: string, workspace: string, number: number) {
  const [run, setRun] = useState<RunType>();
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [error, setError] = useState<string>();

  useEffect(() => {
    const abort = new AbortController();
    let cursor = 0n;
    let done = false;
    setRun(undefined);
    setEvents([]);
    (async () => {
      while (!abort.signal.aborted && !done) {
        try {
          for await (const res of runApi.watchRun({ org, project, workspace, number, afterSeq: cursor }, { signal: abort.signal })) {
            if (res.run) {
              setRun(res.run);
              done = finished(res.run.status);
            }
            if (res.events.length > 0) {
              cursor = res.events[res.events.length - 1].seq;
              setEvents((prev) => [...prev, ...res.events]);
            }
            setError(undefined);
          }
        } catch (err) {
          if (abort.signal.aborted) return;
          setError(message(err));
        }
        if (!done) await new Promise((r) => setTimeout(r, 1000));
      }
    })();
    return () => abort.abort();
  }, [org, project, workspace, number]);

  return { run, events, error, setRun };
}

export function Run() {
  const params = useParams({ strict: false }) as { org: string; project: string; workspace: string; number: string };
  const { org, project, workspace } = params;
  const number = Number(params.number);
  const { me, reloadWaiting } = useShell();
  const { run, events, error, setRun } = useRun(org, project, workspace, number);
  const [actionError, setActionError] = useState<string>();
  const role = me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
  const [full, setFull] = useState(false);
  const [noGraph, setNoGraph] = useState(false);
  const [timelineWidth, setTimelineWidth] = useSavedWidth("stackwright.width.timeline");
  const timelinePane = useRef<HTMLOListElement>(null);
  // The timeline follows new events while it is scrolled to its end, and
  // stops when someone scrolls up to read.
  const follow = useRef(true);
  useEffect(() => {
    const pane = timelinePane.current;
    if (pane && follow.current) pane.scrollTop = pane.scrollHeight;
  }, [events.length]);
  useEffect(() => {
    if (!full) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setFull(false);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [full]);

  useEffect(() => reloadWaiting(), [run?.status, reloadWaiting]);

  const act = async (fn: () => Promise<{ run?: RunType }>) => {
    setActionError(undefined);
    try {
      const res = await fn();
      if (res.run) setRun(res.run);
      reloadWaiting();
      return true;
    } catch (err) {
      setActionError(message(err));
      return false;
    }
  };
  const target = { org, project, workspace, number };
  const hasGraph = !!run && run.status !== RunStatus.QUEUED && run.status !== RunStatus.PLANNING;
  const showTimeline = !hasGraph || !full;
  const showGraph = hasGraph && !noGraph;

  return (
    <div className="run-view">
      <div className="view-bar">
        <span className="title">{run ? runTitle(run) : `Run #${number}`}</span>
        {run && (
          <>
            <span className={`pill ${statusOf(run.status).tone}`}>{statusOf(run.status).text}</span>
            {run.hasChanges && (
              <span className="mono">
                {changes(run).map((c) => (
                  <span key={c.text} className={c.tone}>
                    {c.text}{" "}
                  </span>
                ))}
              </span>
            )}
            <span className="muted">
              {origin(run)}
              {run.createdBy && ` · started by ${run.createdBy}`}
              {run.buildServer && ` · on build server ${run.buildServer}`}
            </span>
          </>
        )}
        {hasGraph && (
          <>
            <span className="grow" />
            <button
              type="button"
              aria-controls="timeline"
              aria-expanded={!full}
              title="Esc shows it again"
              onClick={() => setFull(!full)}
            >
              {full ? "Show timeline" : "Hide timeline"}
            </button>
            <button
              type="button"
              aria-controls="run-graph"
              aria-expanded={!noGraph}
              onClick={() => setNoGraph(!noGraph)}
            >
              {noGraph ? "Show graph" : "Hide graph"}
            </button>
          </>
        )}
      </div>

      {error && <p className="notice error">{error} Reconnecting…</p>}
      <div className={showTimeline ? "run-split" : "run-split full"}>
        {showTimeline && (
          <ol
            id="timeline"
            ref={timelinePane}
            className="timeline"
            aria-label="Timeline"
            aria-live="polite"
            style={showGraph ? paneWidth(timelineWidth) : undefined}
            onScroll={(e) => {
              const pane = e.currentTarget;
              follow.current = pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 24;
            }}
          >
            {events.map((e) => {
              const line = describe(e);
              if (!line.text.trim()) return null;
              return (
                <li key={String(e.seq)} className={line.tone}>
                  <time className="mono muted" dateTime={e.at ? timestampDate(e.at).toISOString() : undefined}>
                    {e.at ? timestampDate(e.at).toLocaleTimeString() : ""}
                  </time>
                  <span>
                    {line.text}
                    {e.resourceAddr && !line.text.includes(e.resourceAddr) && <code> {e.resourceAddr}</code>}
                    {line.detail && <span className="detail">{line.detail}</span>}
                    {line.fix && <AskAgent org={org} project={project} prompt={`Run #${number} hit this error. Fix the code.\n\n${line.fix}`} />}
                  </span>
                </li>
              );
            })}
            {events.length === 0 && <li className="muted">No events yet.</li>}
          </ol>
        )}
        {showTimeline && showGraph && (
          <Divider
            label="Resize the timeline"
            pane={() => timelinePane.current}
            edge="end"
            min={200}
            keep={320}
            onResize={setTimelineWidth}
          />
        )}
        {!showTimeline && !showGraph && <p className="notice muted">The timeline and the graph are hidden.</p>}
        {run && showGraph && (
          <RunGraph
            org={org}
            project={project}
            workspace={workspace}
            run={run}
            events={events}
            writer={role >= Role.WRITER}
            full={full}
          />
        )}
      </div>

      {run && <RunFindings org={org} project={project} workspace={workspace} number={number} status={run.status} />}
      {run && (
        <div className="next-bar">
          <NextStep run={run} role={role} />
          {actionError && <span className="error">{actionError}</span>}
          <span className="grow" />
          {run.status === RunStatus.BLOCKED && role >= Role.REVIEWER && (
            <Action run={() => act(() => runApi.rejectRun(target))} busy="Rejecting…" done="Rejected">
              Reject
            </Action>
          )}
          {run.status === RunStatus.NEEDS_APPROVAL && role >= Role.REVIEWER && (
            <>
              <Action run={() => act(() => runApi.rejectRun(target))} busy="Rejecting…" done="Rejected">
                Reject
              </Action>
              <Action className="primary" run={() => act(() => runApi.approveRun(target))} busy="Approving…" done="Approved">
                Approve and apply
              </Action>
            </>
          )}
          {!finished(run.status) && role >= Role.WRITER && (
            <Action className="danger" run={() => act(() => runApi.cancelRun(target))} busy="Cancelling…" done="Cancelled">
              Cancel run
            </Action>
          )}
        </div>
      )}
    </div>
  );
}

const speeds = ["1", "2", "4", "10"].map((v) => ({ value: v, label: `${v}x` }));

const clock = (ms: number) => {
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
};

const isApply = (e: RunEvent) => {
  if (e.type !== "run.dispatched") return false;
  try {
    return JSON.parse(e.payload).operation === "apply";
  } catch {
    return false;
  }
};

type RunGraphProps = {
  org: string;
  project: string;
  workspace: string;
  run: RunType;
  events: RunEvent[];
  writer: boolean;
  // full hides the timeline, so the graph takes the page.
  full: boolean;
};

// RunGraph shows the run's plan graph. During the apply its nodes follow
// the events; after the run, the apply can be replayed.
function RunGraph({ org, project, workspace, run, events, writer, full }: RunGraphProps) {
  const graph = useLoad(`plan-graph:${org}/${project}/${workspace}/${run.number}`, () =>
    graphApi.getPlanGraph({ org, project, workspace, number: run.number }),
  );
  const applyEvents = useMemo(() => {
    let start = -1;
    events.forEach((e, i) => {
      if (isApply(e)) start = i;
    });
    return start < 0 ? [] : events.slice(start);
  }, [events]);
  const times = useMemo(() => timeline(applyEvents), [applyEvents]);
  const total = times.at(-1) ?? 0;
  // The replay's position in ms; undefined shows the run as it stands.
  const [at, setAt] = useState<number>();
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState("1");
  const shown = at === undefined ? applyEvents.length : times.filter((t) => t <= at).length;
  const state = useMemo(() => applyEvents.slice(0, shown).reduce(step, emptyState()), [applyEvents, shown]);
  const g = graph.data?.graph;
  const states = useMemo(() => (g && state.started ? statuses(g, state) : undefined), [g, state]);

  useEffect(() => {
    if (!playing) return;
    let frame = 0;
    let last = performance.now();
    const tick = (now: number) => {
      const dt = now - last;
      last = now;
      setAt((t) => Math.min((t ?? 0) + dt * Number(speed), total));
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [playing, speed, total]);
  useEffect(() => {
    if (playing && at !== undefined && at >= total) setPlaying(false);
  }, [playing, at, total]);

  if (graph.error || !graph.data) return null;
  const replayable = finished(run.status) && applyEvents.length > 0;
  const progress = replayable ? (
    <span className="replay row">
      <button
        type="button"
        onClick={() => {
          if (!playing && (at === undefined || at >= total)) setAt(0);
          setPlaying(!playing);
        }}
      >
        {playing ? "Pause" : "Replay"}
      </button>
      <input
        type="range"
        aria-label="Replay position"
        min={0}
        max={total}
        value={at ?? total}
        onChange={(e) => {
          setPlaying(false);
          setAt(Number(e.target.value));
        }}
      />
      <Select aria-label="Replay speed" value={speed} onChange={setSpeed} options={speeds} />
      <span className="mono muted">
        {clock(at ?? total)} / {clock(total)}
      </span>
    </span>
  ) : (
    applyEvents.length > 0 && <span className="live">Applying, live</span>
  );
  return (
    <div id="run-graph" className="plan-view run-graph">
      <Graph
        org={org}
        project={project}
        workspace={workspace}
        data={graph.data}
        writer={writer}
        states={states}
        diagnostics={state.diagnostics}
        controls={progress}
        fit={full}
      />
    </div>
  );
}

function NextStep({ run, role }: { run: RunType; role: Role }) {
  switch (run.status) {
    case RunStatus.QUEUED:
      return <span>Waits for the run before it in this workspace.</span>;
    case RunStatus.PLANNING:
      return <span>Planning. Nothing changes until someone approves the plan.</span>;
    case RunStatus.NEEDS_APPROVAL:
      return role >= Role.REVIEWER ? (
        <span>Apply these changes? The apply uses this plan only.</span>
      ) : (
        <span>Waits for a reviewer to approve or reject it.</span>
      );
    case RunStatus.BLOCKED:
      return (
        <span className="error">
          ! A High or Critical finding blocks the apply. Fix the code, or accept the risk to send the plan for approval.
        </span>
      );
    case RunStatus.APPLYING:
      return <span>Applying the approved plan.</span>;
    case RunStatus.PLANNED:
      return <span>{run.hasChanges ? "A plan only. Start a run to apply it." : "No changes. Nothing to apply."}</span>;
    case RunStatus.APPLIED:
      return <span className="ok">✓ Applied.</span>;
    case RunStatus.FAILED:
      return <span className="error">! {run.error || "The run failed."}</span>;
    case RunStatus.REJECTED:
      return <span>Rejected by {run.decidedBy}. Nothing was applied.</span>;
    case RunStatus.CANCELED:
      return <span>Canceled by {run.decidedBy}.</span>;
  }
  return null;
}

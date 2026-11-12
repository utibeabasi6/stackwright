import { useEffect, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { message, runApi } from "../api";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { RunStatus, type Run as RunType, type RunEvent } from "../gen/stackwright/v1/runs_pb";
import { useShell } from "../me";
import { changes, describeEvent, finished, origin, runTitle, statusOf } from "../runs";

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
  const { me, openTab, reloadWaiting } = useShell();
  const { run, events, error, setRun } = useRun(org, project, workspace, number);
  const [actionError, setActionError] = useState<string>();
  const role = me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;

  useEffect(() => openTab({ project, workspace }), [project, workspace, openTab]);
  useEffect(() => reloadWaiting(), [run?.status, reloadWaiting]);

  const act = async (fn: () => Promise<{ run?: RunType }>) => {
    setActionError(undefined);
    try {
      const res = await fn();
      if (res.run) setRun(res.run);
      reloadWaiting();
    } catch (err) {
      setActionError(message(err));
    }
  };
  const target = { org, project, workspace, number };

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
        <span className="grow" />
        <Link to="/$org/projects/$project/$workspace/runs" params={{ org, project, workspace }}>
          All runs
        </Link>
      </div>

      {error && <p className="notice error">{error} Reconnecting…</p>}
      <ol className="timeline" aria-label="Timeline" aria-live="polite">
        {events.map((e) => {
          const line = describeEvent(e);
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
              </span>
            </li>
          );
        })}
        {events.length === 0 && <li className="muted">No events yet.</li>}
      </ol>

      {run && (
        <div className="next-bar">
          <NextStep run={run} role={role} />
          {actionError && <span className="error">{actionError}</span>}
          <span className="grow" />
          {run.status === RunStatus.NEEDS_APPROVAL && role >= Role.REVIEWER && (
            <>
              <button type="button" onClick={() => act(() => runApi.rejectRun(target))}>
                Reject
              </button>
              <button type="button" className="primary" onClick={() => act(() => runApi.approveRun(target))}>
                Approve and apply
              </button>
            </>
          )}
          {!finished(run.status) && role >= Role.WRITER && (
            <button type="button" className="danger" onClick={() => act(() => runApi.cancelRun(target))}>
              Cancel run
            </button>
          )}
        </div>
      )}
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

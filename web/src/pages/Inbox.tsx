import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { runApi } from "../api";
import { FindingsTable } from "./Findings";
import type { Run } from "../gen/stackwright/v1/runs_pb";
import { useShell } from "../me";
import { changes, runTitle } from "../runs";
import { useLoad } from "../useLoad";

export function Inbox() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { waitingVersion } = useShell();
  const waiting = useLoad(`waiting:${org}`, () => runApi.listWaiting({ org }), waitingVersion);
  const findings = waiting.data?.findings ?? [];
  const drift = waiting.data?.drift ?? [];
  const deployments = waiting.data?.deployments ?? [];
  const extra = findings.length + drift.length + deployments.length;
  const items = [
    ...(waiting.data?.approvals ?? []).map((run) => ({ kind: "Approval", run })),
    ...(waiting.data?.failures ?? []).map((run) => ({ kind: "Failure", run })),
  ];

  return (
    <div className="stack">
      <div>
        <h1>Waiting on you</h1>
        <p className="muted">
          {items.length + extra === 0
            ? "Nothing waits on you or your team."
            : `${items.length + extra} ${items.length + extra === 1 ? "thing needs" : "things need"} you or your team. Each one leaves this list when someone acts on it.`}
        </p>
      </div>
      {waiting.error && <p className="error">{waiting.error}</p>}
      <ul className="inbox">
        {items.map(({ kind, run }) => (
          <li key={run.id} className="card row">
            <div className="grow stack">
              <span className="hint">
                {kind} · <span className="mono">{`${run.project} / ${run.workspace}`}</span> · {when(run)}
              </span>
              <strong>
                {kind === "Approval" ? (
                  <>
                    {runTitle(run)} waits for approval: <Changes run={run} />
                  </>
                ) : (
                  <>
                    <span className="error">!</span> {runTitle(run)} failed
                  </>
                )}
              </strong>
              {kind === "Failure" && <span className="mono muted">{run.error}</span>}
            </div>
            <Link
              className={kind === "Approval" ? "button primary" : "button"}
              to="/$org/projects/$project/$workspace/runs/$number"
              params={{ org, project: run.project, workspace: run.workspace, number: String(run.number) }}
            >
              {kind === "Approval" ? "Review plan" : "Open run"}
            </Link>
          </li>
        ))}
      </ul>
      {deployments.length > 0 && (
        <ul className="inbox" aria-label="Deployments">
          {deployments.map((d) => (
            <li key={`${d.name}#${d.number}`} className="card row">
              <div className="grow stack">
                <span className="hint">
                  Deployment · <span className="mono">{d.name}</span>
                  {d.since && ` · ${timestampDate(d.since).toLocaleString()}`}
                </span>
                <strong>
                  Run #{d.number} of {d.name} waits for approval
                </strong>
              </div>
              <Link className="button primary" to="/$org/deployments/$name" params={{ org, name: d.name }}>
                Review deployment
              </Link>
            </li>
          ))}
        </ul>
      )}
      {drift.length > 0 && (
        <section className="stack" aria-labelledby="drift-h">
          <h2 id="drift-h">Changed outside Stackwright</h2>
          <FindingsTable org={org} findings={drift} reload={waiting.reload} where />
        </section>
      )}
      {findings.length > 0 && (
        <section className="stack" aria-labelledby="findings-h">
          <h2 id="findings-h">Open High and Critical findings</h2>
          <FindingsTable org={org} findings={findings} reload={waiting.reload} where />
        </section>
      )}
    </div>
  );
}

function when(run: Run) {
  const t = run.finishedAt ?? run.updatedAt;
  return t ? timestampDate(t).toLocaleString() : "";
}

function Changes({ run }: { run: Run }) {
  return (
    <span className="mono">
      {changes(run).map((c) => (
        <span key={c.text} className={c.tone}>
          {c.text}{" "}
        </span>
      ))}
    </span>
  );
}

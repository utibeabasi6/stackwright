import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { runApi } from "../api";
import type { Run } from "../gen/stackwright/v1/runs_pb";
import { useShell } from "../me";
import { changes, runTitle } from "../runs";
import { useLoad } from "../useLoad";

export function Inbox() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { waitingVersion } = useShell();
  const waiting = useLoad(`waiting:${org}:${waitingVersion}`, () => runApi.listWaiting({ org }));
  const items = [
    ...(waiting.data?.approvals ?? []).map((run) => ({ kind: "Approval", run })),
    ...(waiting.data?.failures ?? []).map((run) => ({ kind: "Failure", run })),
  ];

  return (
    <div className="stack">
      <div>
        <h1>Waiting on you</h1>
        <p className="muted">
          {items.length === 0
            ? "Nothing waits on you or your team."
            : `${items.length} ${items.length === 1 ? "thing needs" : "things need"} you or your team. Each one leaves this list when someone acts on it.`}
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

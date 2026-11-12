import { useEffect } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { runApi } from "../api";
import { useShell } from "../me";
import { origin, runTitle, statusOf } from "../runs";
import { useLoad } from "../useLoad";

export function Runs() {
  const { org, project, workspace } = useParams({ strict: false }) as { org: string; project: string; workspace: string };
  const { openTab } = useShell();
  const runs = useLoad(`runs:${org}/${project}/${workspace}`, () => runApi.listRuns({ org, project, workspace }).then((r) => r.runs));
  useEffect(() => openTab({ project, workspace }), [project, workspace, openTab]);

  return (
    <div className="stack">
      <h1>
        Runs <span className="muted mono">{`${project} › ${workspace}`}</span>
      </h1>
      {runs.error && <p className="error">{runs.error}</p>}
      {runs.data?.length === 0 && <p className="muted">No runs yet. Start one from the Overview.</p>}
      {(runs.data?.length ?? 0) > 0 && (
        <div className="table-box">
          <table className="table">
            <thead>
              <tr>
                <th>Run</th>
                <th>Status</th>
                <th>Changes</th>
                <th>Code</th>
                <th>Started</th>
              </tr>
            </thead>
            <tbody>
              {runs.data?.map((r) => (
                <tr key={r.id}>
                  <td>
                    <Link to="/$org/projects/$project/$workspace/runs/$number" params={{ org, project, workspace, number: String(r.number) }}>
                      {runTitle(r)}
                    </Link>
                  </td>
                  <td className={statusOf(r.status).tone}>{statusOf(r.status).text}</td>
                  <td className="mono">{r.hasChanges ? `+${r.adds} ~${r.changes} −${r.destroys}` : ""}</td>
                  <td>{origin(r)}</td>
                  <td>
                    {r.createdAt ? timestampDate(r.createdAt).toLocaleString() : ""}
                    {r.createdBy && <span className="muted"> · {r.createdBy}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

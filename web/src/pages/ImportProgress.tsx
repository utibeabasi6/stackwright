import { useEffect, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { importApi, message } from "../api";
import type { Import } from "../gen/stackwright/v1/imports_pb";
import { RunStatus } from "../gen/stackwright/v1/runs_pb";

const finalStatus = new Set([RunStatus.PLANNED, RunStatus.FAILED, RunStatus.CANCELED, RunStatus.APPLIED, RunStatus.REJECTED]);

// ImportProgress shows what an import made and each first plan. It reads
// the import again until every plan ends, so a reload shows the same.
export function ImportProgress() {
  const { org, id } = useParams({ strict: false }) as { org: string; id: string };
  const [imp, setImp] = useState<Import>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const { import: i } = await importApi.getImport({ org, id });
        if (stopped || !i) return;
        setImp(i);
        if (i.workspaces.some((w) => w.runNumber && !finalStatus.has(w.runStatus))) timer = setTimeout(load, 2000);
      } catch (e) {
        if (!stopped) setError(message(e));
      }
    };
    void load();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [org, id]);

  if (error) return <p className="error">{error}</p>;
  if (!imp) return null;
  const planned = imp.workspaces.filter((w) => w.runNumber);
  const clean = planned.filter((w) => w.runStatus === RunStatus.PLANNED && !w.hasChanges).length;
  const waiting = planned.filter((w) => !finalStatus.has(w.runStatus)).length;
  return (
    <div className="stack">
      <h1>Import of {imp.repo}</h1>
      <p className="muted">
        At <span className="mono">{imp.commit.slice(0, 7)}</span>
        {imp.createdBy && `, by ${imp.createdBy}`}
        {imp.createdAt && `, ${timestampDate(imp.createdAt).toLocaleString()}`}.
      </p>
      <p role="status">
        {planned.length === 0
          ? "Every folder was imported before; this import made nothing new."
          : waiting > 0
            ? `${planned.length - waiting} of ${planned.length} first plans done.`
            : `${clean} of ${planned.length} first plans show no changes.`}
      </p>
      <div className="table-box">
        <table className="table">
          <thead>
            <tr>
              <th>Project</th>
              <th>Workspace</th>
              <th>First plan</th>
            </tr>
          </thead>
          <tbody>
            {imp.workspaces.map((w) => (
              <tr key={`${w.project}/${w.workspace}`}>
                <td className="mono">
                  <Link to="/$org/projects/$project/$workspace" params={{ org, project: w.project, workspace: w.workspace }}>
                    {w.project}
                  </Link>
                </td>
                <td className="mono">{w.workspace}</td>
                <td>
                  {!w.runNumber ? (
                    <span className="muted">Imported before</span>
                  ) : (
                    <Link to="/$org/projects/$project/$workspace/runs/$number" params={{ org, project: w.project, workspace: w.workspace, number: String(w.runNumber) }}>
                      #{w.runNumber}{" "}
                      {w.runStatus === RunStatus.PLANNED
                        ? w.hasChanges
                          ? "has changes: check that the state is the right one"
                          : "✓ no changes"
                        : w.runStatus === RunStatus.FAILED
                          ? `failed: ${w.runError}`
                          : "planning…"}
                    </Link>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

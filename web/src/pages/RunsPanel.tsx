import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { message, projectApi, runApi } from "../api";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { RunStatus, type Run } from "../gen/stackwright/v1/runs_pb";
import { formatLabels, parseLabels } from "../labels";
import { useShell } from "../me";
import { origin, runTitle, statusOf } from "../runs";
import { useLoad } from "../useLoad";

type Props = {
  org: string;
  project: string;
  workspace: string;
  sample: boolean;
  linkedRepo: string;
  runBranch: string;
  autoApply: boolean;
  requiredLabels: Record<string, string>;
  reload: () => void;
};

export function RunsPanel({ org, project, workspace, sample, linkedRepo, runBranch, autoApply, requiredLabels, reload }: Props) {
  const { me, reloadWaiting } = useShell();
  const navigate = useNavigate();
  const runs = useLoad(`runs:${org}/${project}/${workspace}`, () => runApi.listRuns({ org, project, workspace }).then((r) => r.runs));
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const role = me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
  const latest = runs.data?.[0];
  const urgent = latest?.status === RunStatus.NEEDS_APPROVAL || latest?.status === RunStatus.FAILED;

  const start = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    const file = f.get("source");
    setError(undefined);
    setBusy(true);
    try {
      const upload = file instanceof File && file.size > 0 ? file : undefined;
      const { run } = await runApi.createRun({
        org,
        project,
        workspace,
        planOnly: f.get("planOnly") === "on",
        source: upload ? new Uint8Array(await upload.arrayBuffer()) : new Uint8Array(),
        sourceName: upload?.name ?? "",
      });
      reloadWaiting();
      if (run) navigate({ to: "/$org/projects/$project/$workspace/runs/$number", params: { org, project, workspace, number: String(run.number) } });
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  const update = async (change: Parameters<typeof projectApi.updateWorkspace>[0]) => {
    setError(undefined);
    try {
      await projectApi.updateWorkspace({ org, project, workspace, ...change });
      reload();
    } catch (err) {
      setError(message(err));
    }
  };
  const saveLabels = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    void update({ requiredLabels: { values: parseLabels(String(new FormData(e.currentTarget).get("labels"))) } });
  };
  const labels = formatLabels(requiredLabels);

  return (
    <>
      <NextStep org={org} project={project} workspace={workspace} latest={latest} urgent={urgent} />
      <div className="cards">
        <section className="card stack" aria-labelledby="runs-h">
          <h2 id="runs-h">Recent runs</h2>
          {runs.data?.length === 0 && <p className="muted">No runs yet.</p>}
          <ul className="rows">
            {runs.data?.slice(0, 5).map((r) => (
              <li key={r.id}>
                <Link to="/$org/projects/$project/$workspace/runs/$number" params={{ org, project, workspace, number: String(r.number) }}>
                  <span>
                    <span className="mono">#{r.number}</span> {r.planOnly ? "plan only" : "plan and apply"} · {origin(r)}
                  </span>
                  <span className={statusOf(r.status).tone}>{statusOf(r.status).text}</span>
                </Link>
              </li>
            ))}
          </ul>
          {(runs.data?.length ?? 0) > 5 && (
            <Link to="/$org/projects/$project/$workspace/runs" params={{ org, project, workspace }}>
              See all runs
            </Link>
          )}
        </section>

        {role >= Role.WRITER && (
          <section className="card" aria-labelledby="start-h">
            <form className="stack" onSubmit={start}>
              <h2 id="start-h">Start a run</h2>
              {linkedRepo && (
                <p className="muted">
                  Plans the newest commit of <span className="mono">{runBranch}</span> in{" "}
                  <span className="mono">{linkedRepo}</span>. Pull requests and pushes start runs on their own.
                </p>
              )}
              {!sample && !linkedRepo && (
                <label className="field">
                  <span>Code, as a .tar.gz of the repository</span>
                  <input name="source" type="file" accept=".tar.gz,.tgz,application/gzip" required />
                </label>
              )}
              {sample && <p className="muted">This sample project runs its own code. You can also upload a .tar.gz.</p>}
              {sample && (
                <label className="field">
                  <span>Code (optional)</span>
                  <input name="source" type="file" accept=".tar.gz,.tgz,application/gzip" />
                </label>
              )}
              <label className="check">
                <input name="planOnly" type="checkbox" /> Plan only, never apply
              </label>
              <p className="hint">
                {autoApply ? "Auto-apply is on: a plan with changes applies with no approval." : "A plan with changes waits for a reviewer."}{" "}
                {labels ? `Runs go only to build servers with ${labels}.` : "Runs go to any build server."}
              </p>
              {error && <p className="error">{error}</p>}
              <div className="row">
                <button type="submit" className={urgent ? undefined : "primary"} disabled={busy}>
                  Start plan
                </button>
              </div>
            </form>
            {role >= Role.ADMIN && (
              <>
                <label className="check">
                  <input type="checkbox" checked={autoApply} onChange={(e) => update({ autoApply: e.target.checked })} /> Auto-apply plans
                  in this workspace
                </label>
                <form className="row" onSubmit={saveLabels} key={labels}>
                  <label className="field grow">
                    <span>Build server labels this workspace requires</span>
                    <input name="labels" defaultValue={labels} placeholder="region=eu" className="mono" />
                  </label>
                  <button type="submit">Save labels</button>
                </form>
              </>
            )}
          </section>
        )}
      </div>
    </>
  );
}

type NextProps = { org: string; project: string; workspace: string; latest?: Run; urgent: boolean };

function NextStep({ org, project, workspace, latest, urgent }: NextProps) {
  const link = latest && (
    <Link
      className={urgent ? "button primary" : "button"}
      to="/$org/projects/$project/$workspace/runs/$number"
      params={{ org, project, workspace, number: String(latest.number) }}
    >
      Open {runTitle(latest).toLowerCase()}
    </Link>
  );
  let title = "Nothing waits on you here";
  let body = "Start a plan to see what your code would change.";
  let show = true;
  switch (latest?.status) {
    case RunStatus.NEEDS_APPROVAL:
      title = `${runTitle(latest)} waits for approval`;
      body = `It plans +${latest.adds} ~${latest.changes} −${latest.destroys}. Nothing changes until a reviewer approves it.`;
      break;
    case RunStatus.FAILED:
      title = `${runTitle(latest)} failed`;
      body = latest.error;
      break;
    case RunStatus.QUEUED:
    case RunStatus.PLANNING:
    case RunStatus.APPLYING:
      title = `${runTitle(latest)} is ${statusOf(latest.status).text.toLowerCase()}`;
      body = "Its timeline shows each step as it happens.";
      break;
    default:
      show = false;
  }
  return (
    <section className="card next" aria-labelledby="next-h">
      <div className="row">
        <div className="grow stack">
          <span className="label">Next step</span>
          <h2 id="next-h">{title}</h2>
          <p className="muted">{body}</p>
        </div>
        {show && link}
      </div>
    </section>
  );
}

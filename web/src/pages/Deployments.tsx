import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Action, Submit, useAction } from "../Action";
import { deploymentApi } from "../api";
import { DeploymentRunStatus, type DeploymentLink, type DeploymentRun, type WorkspaceRef } from "../gen/stackwright/v1/deployments_pb";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useShell } from "../me";
import { statusOf } from "../runs";
import { Select } from "../Select";
import { useLoad } from "../useLoad";

const runText: Record<DeploymentRunStatus, { text: string; tone: string }> = {
  [DeploymentRunStatus.UNSPECIFIED]: { text: "Unknown", tone: "muted" },
  [DeploymentRunStatus.PLANNING]: { text: "Planning", tone: "muted" },
  [DeploymentRunStatus.NEEDS_APPROVAL]: { text: "~ Needs approval", tone: "warn" },
  [DeploymentRunStatus.APPLYING]: { text: "Applying", tone: "muted" },
  [DeploymentRunStatus.APPLIED]: { text: "✓ Applied", tone: "ok" },
  [DeploymentRunStatus.PLANNED]: { text: "✓ No changes", tone: "ok" },
  [DeploymentRunStatus.FAILED]: { text: "! Failed", tone: "error" },
  [DeploymentRunStatus.REJECTED]: { text: "Rejected", tone: "muted" },
  [DeploymentRunStatus.CANCELED]: { text: "Canceled", tone: "muted" },
};

const active = (s?: DeploymentRunStatus) =>
  s === DeploymentRunStatus.PLANNING || s === DeploymentRunStatus.NEEDS_APPROVAL || s === DeploymentRunStatus.APPLYING;

const refText = (r?: WorkspaceRef) => (r ? `${r.project}/${r.workspace}` : "");

function useRole(org: string) {
  const { me } = useShell();
  return me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
}

// Deployments lists the org's deployments (adr/0042).
export function Deployments() {
  const { org } = useParams({ strict: false }) as { org: string };
  const navigate = useNavigate();
  const list = useLoad(`deployments:${org}`, () => deploymentApi.listDeployments({ org }).then((r) => r.deployments));
  const writer = useRole(org) >= Role.WRITER;
  const [name, setName] = useState("");
  return (
    <div className="stack">
      <div>
        <h1>Deployments</h1>
        <p className="muted">
          A deployment applies several workspaces in order with one approval. Linking one step's output to another step's variable also
          orders them.
        </p>
      </div>
      {list.error && <p className="error">{list.error}</p>}
      {list.data?.length === 0 && <p className="muted">No deployments yet.</p>}
      {(list.data?.length ?? 0) > 0 && (
        <div className="table-box">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Steps, in order</th>
                <th>Last run</th>
              </tr>
            </thead>
            <tbody>
              {list.data?.map((d) => (
                <tr key={d.name}>
                  <td className="mono">
                    <Link to="/$org/deployments/$name" params={{ org, name: d.name }}>
                      {d.name}
                    </Link>
                    {d.stackProject && <span className="muted"> · Stack {d.stackProject}</span>}
                  </td>
                  <td className="mono">{d.steps.map(refText).join(" → ")}</td>
                  <td>
                    {d.lastRun ? (
                      <span className={runText[d.lastRun.status].tone}>
                        #{d.lastRun.number} {runText[d.lastRun.status].text}
                      </span>
                    ) : (
                      <span className="muted">Never run</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {writer && (
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault();
            void navigate({ to: "/$org/deployments/$name", params: { org, name } });
          }}
        >
          <label className="field">
            New deployment
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="platform" pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?" required />
          </label>
          <button type="submit">Create</button>
        </form>
      )}
    </div>
  );
}

// DeploymentPage edits a deployment's steps and links, and shows its runs.
export function DeploymentPage() {
  const { org, name } = useParams({ strict: false }) as { org: string; name: string };
  const role = useRole(org);
  const [version, setVersion] = useState(0);
  const got = useLoad(
    `deployment:${org}/${name}`,
    () => deploymentApi.getDeployment({ org, name }).catch(() => undefined),
    version,
  );
  const latest = got.data?.runs[0];
  useEffect(() => {
    if (!active(latest?.status)) return;
    const t = setTimeout(() => setVersion((v) => v + 1), 2000);
    return () => clearTimeout(t);
  }, [latest, version]);
  const reload = () => setVersion((v) => v + 1);
  const target = { org, name, number: latest?.number ?? 0 };
  const stack = got.data?.deployment?.stackProject ? got.data.deployment : undefined;

  return (
    <div className="stack">
      <h1 className="mono">{name}</h1>
      {stack ? (
        <section className="card stack" aria-label="Steps">
          <p>
            The Terraform Stack in project{" "}
            <Link to="/$org/projects/$project" params={{ org, project: stack.stackProject }} className="mono">
              {stack.stackProject}
            </Link>{" "}
            defines this deployment as <span className="mono">{stack.stackDeployment}</span>
            {stack.stackGroup && (
              <>
                , in group <span className="mono">{stack.stackGroup}</span>
              </>
            )}
            . Change the Stack&apos;s files to change it; a push deploys it.
          </p>
          <ol className="rows">
            {stack.steps.map((s) => (
              <li key={refText(s)} className="mono">
                {s.workspace}
              </li>
            ))}
          </ol>
        </section>
      ) : (
        <Editor org={org} name={name} steps={got.data?.deployment?.steps ?? []} links={got.data?.deployment?.links ?? []} writer={role >= Role.WRITER}
          locked={active(latest?.status)} saved={reload} key={got.data ? "loaded" : "new"} />
      )}
      {got.data?.deployment && (
        <section className="stack" aria-labelledby="runs-h">
          <div className="row">
            <h2 id="runs-h" className="grow">
              Runs
            </h2>
            {role >= Role.WRITER && !active(latest?.status) && (
              <>
                <Action run={() => deploymentApi.startDeploymentRun({ org, name }).then(reload)} busy="Starting…" done="Started" className="primary">
                  Deploy
                </Action>
                <Action
                  className="danger"
                  confirm={{ message: `Destroy every step of ${name}, in reverse order? Nothing applies until the run is approved.`, action: "Plan destroy", danger: true }}
                  run={() => deploymentApi.startDeploymentRun({ org, name, destroy: true }).then(reload)}
                  busy="Starting…"
                  done="Started"
                >
                  Destroy
                </Action>
              </>
            )}
          </div>
          {latest && <RunCard org={org} run={latest} />}
          {latest?.status === DeploymentRunStatus.NEEDS_APPROVAL && role >= Role.REVIEWER && (
            <div className="row">
              <Action className="primary" run={() => deploymentApi.approveDeploymentRun(target).then(reload)} busy="Approving…" done="Approved">
                Approve and apply every step
              </Action>
              <Action run={() => deploymentApi.rejectDeploymentRun(target).then(reload)} busy="Rejecting…" done="Rejected">
                Reject
              </Action>
            </div>
          )}
          {active(latest?.status) && role >= Role.WRITER && (
            <div>
              <Action className="danger" run={() => deploymentApi.cancelDeploymentRun(target).then(reload)} busy="Cancelling…" done="Cancelled">
                Cancel the deployment run
              </Action>
            </div>
          )}
          {(got.data.runs.length ?? 0) > 1 && (
            <details>
              <summary>Earlier runs</summary>
              <ul>
                {got.data.runs.slice(1).map((r) => (
                  <li key={r.number}>
                    #{r.number} <span className={runText[r.status].tone}>{runText[r.status].text}</span>
                    {r.destroy && " (destroy)"}
                    {r.createdAt && <span className="muted"> · {timestampDate(r.createdAt).toLocaleString()}</span>}
                    {r.error && <span className="error"> · {r.error}</span>}
                  </li>
                ))}
              </ul>
            </details>
          )}
        </section>
      )}
    </div>
  );
}

function RunCard({ org, run }: { org: string; run: DeploymentRun }) {
  return (
    <div className="card stack" aria-label={`Deployment run ${run.number}`}>
      <div>
        <strong>
          Run #{run.number}
          {run.destroy && " · destroy"}
        </strong>{" "}
        <span className={`pill ${runText[run.status].tone}`}>{runText[run.status].text}</span>
        {run.createdBy && <span className="muted"> · started by {run.createdBy}</span>}
        {run.decidedBy && <span className="muted"> · decided by {run.decidedBy}</span>}
      </div>
      {run.error && <p className="error">! {run.error}</p>}
      <ol className="deployment-steps">
        {run.steps.map((s) => (
          <li key={refText(s.workspace)}>
            <span className="mono">{refText(s.workspace)}</span>{" "}
            {s.runNumber ? (
              <Link
                to="/$org/projects/$project/$workspace/runs/$number"
                params={{ org, project: s.workspace!.project, workspace: s.workspace!.workspace, number: String(s.runNumber) }}
              >
                #{s.runNumber} {statusOf(s.runStatus).text}
              </Link>
            ) : (
              <span className="muted">
                {s.waitingFor.length > 0 ? `waits for ${s.waitingFor.map(refText).join(", ")}` : "waits for its turn"}
              </span>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}

type EditorProps = { org: string; name: string; steps: WorkspaceRef[]; links: DeploymentLink[]; writer: boolean; locked: boolean; saved: () => void };

function Editor({ org, name, steps: initialSteps, links: initialLinks, writer, locked, saved }: EditorProps) {
  const { projects } = useShell();
  const options = (projects ?? []).flatMap((p) => p.workspaces.map((w) => ({ value: `${p.slug}/${w.name}`, label: `${p.slug}/${w.name}` })));
  const [steps, setSteps] = useState(initialSteps.map(refText));
  const [links, setLinks] = useState(
    initialLinks.map((l) => ({ from: refText(l.from), to: refText(l.to), output: l.output, variable: l.variable })),
  );
  const [pick, setPick] = useState("");
  const saving = useAction();
  const ref = (s: string) => ({ project: s.slice(0, s.lastIndexOf("/")), workspace: s.slice(s.lastIndexOf("/") + 1) });
  const stepOptions = steps.map((s) => ({ value: s, label: s }));
  const save = (e: FormEvent) => {
    e.preventDefault();
    void saving.run(async () => {
      await deploymentApi.saveDeployment({
        org,
        name,
        steps: steps.map(ref),
        links: links.map((l) => ({ from: ref(l.from), to: ref(l.to), output: l.output.trim(), variable: l.variable.trim() })),
      });
      saved();
    });
  };
  const disabled = !writer || locked;
  return (
    <form className="card stack" onSubmit={save} aria-label="Steps and links">
      <h2>Steps</h2>
      {steps.length === 0 && <p className="muted">Add the workspaces this deployment applies.</p>}
      <ul className="rows">
        {steps.map((s) => (
          <li key={s} className="row">
            <span className="mono grow">{s}</span>
            {!disabled && (
              <button
                type="button"
                className="link"
                onClick={() => {
                  setSteps(steps.filter((x) => x !== s));
                  setLinks(links.filter((l) => l.from !== s && l.to !== s));
                }}
              >
                Remove
              </button>
            )}
          </li>
        ))}
      </ul>
      {!disabled && (
        <div className="row">
          <Select aria-label="Workspace to add" value={pick} onChange={setPick} options={[{ value: "", label: "Choose a workspace" }, ...options.filter((o) => !steps.includes(o.value))]} />
          <button type="button" disabled={!pick} onClick={() => (setSteps([...steps, pick]), setPick(""))}>
            Add step
          </button>
        </div>
      )}
      <h2>Links</h2>
      <p className="hint">A link runs the second step after the first. With an output, the second step gets it as the variable.</p>
      {links.map((l, i) => (
        <div key={i} className="row">
          <Select aria-label="From" value={l.from} onChange={(v) => setLinks(links.map((x, j) => (j === i ? { ...x, from: v } : x)))} options={stepOptions} disabled={disabled} />
          <input aria-label="Output" placeholder="output" value={l.output} disabled={disabled} onChange={(e) => setLinks(links.map((x, j) => (j === i ? { ...x, output: e.target.value } : x)))} />
          <span aria-hidden="true">→</span>
          <Select aria-label="To" value={l.to} onChange={(v) => setLinks(links.map((x, j) => (j === i ? { ...x, to: v } : x)))} options={stepOptions} disabled={disabled} />
          <input aria-label="Variable" placeholder="variable" value={l.variable} disabled={disabled} onChange={(e) => setLinks(links.map((x, j) => (j === i ? { ...x, variable: e.target.value } : x)))} />
          {!disabled && (
            <button type="button" className="link" onClick={() => setLinks(links.filter((_, j) => j !== i))}>
              Remove
            </button>
          )}
        </div>
      ))}
      {!disabled && steps.length >= 2 && (
        <div>
          <button type="button" onClick={() => setLinks([...links, { from: steps[0], to: steps[1], output: "", variable: "" }])}>
            Add link
          </button>
        </div>
      )}
      {locked && <p className="hint">A run is under way; change the deployment when it ends.</p>}
      {!disabled && (
        <div>
          <Submit action={saving} className="primary" busy="Saving…" done="Saved" disabled={steps.length === 0}>
            Save deployment
          </Submit>
        </div>
      )}
    </form>
  );
}

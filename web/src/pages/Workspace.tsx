import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { message, projectApi } from "../api";
import { describe, engineName } from "../engines";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { Layout } from "../gen/stackwright/v1/projects_pb";
import { useShell } from "../me";
import { useLoad } from "../useLoad";
import { RunsPanel } from "./RunsPanel";
import { StatePanel } from "./StatePanel";

export function Workspace() {
  const { org, project, workspace } = useParams({ strict: false }) as { org: string; project: string; workspace: string };
  const { me, openTab, reloadProjects } = useShell();
  const navigate = useNavigate();
  const ws = useLoad(`${org}/${project}/${workspace}`, () => projectApi.getWorkspace({ org, project, workspace }));
  const [error, setError] = useState<string>();
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;

  useEffect(() => {
    if (ws.data) openTab({ project, workspace });
  }, [ws.data, project, workspace, openTab]);

  if (ws.error) {
    return (
      <div className="stack">
        <h1>Workspace not found</h1>
        <p className="muted">
          {project} › {workspace} does not exist in this org, or it was archived.
        </p>
      </div>
    );
  }
  const p = ws.data?.project;
  if (!p) return null;

  const act = async (fn: () => Promise<unknown>, after?: () => void) => {
    setError(undefined);
    try {
      await fn();
      reloadProjects();
      ws.reload();
      after?.();
    } catch (err) {
      setError(message(err));
    }
  };
  const save = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    act(() =>
      projectApi.updateProject({
        org,
        project,
        path: String(f.get("path")),
        engineVersion: String(f.get("version")),
        repoUrl: String(f.get("repoUrl")),
        branch: String(f.get("branch")),
      }),
    );
  };
  const addWorkspace = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const name = String(new FormData(form).get("name"));
    act(() => projectApi.createWorkspace({ org, project, name }), () => form.reset());
  };

  return (
    <div className="stack">
      <div>
        <h1 className="mono" style={{ marginBottom: 4 }}>
          {workspace}
        </h1>
        <span className="muted">
          {project} · {describe(p)}
        </span>
      </div>

      <RunsPanel
        org={org}
        project={project}
        workspace={workspace}
        sample={p.sample}
        autoApply={ws.data?.workspace?.autoApply ?? false}
        requiredLabels={ws.data?.workspace?.requiredLabels ?? {}}
        reload={ws.reload}
      />

      <StatePanel org={org} project={project} workspace={workspace} engine={p.engine} admin={admin} />

      {error && <p className="error">{error}</p>}

      <div className="cards">
        <section className="card" aria-labelledby="details-h">
          <h2 id="details-h">Details</h2>
          <dl className="facts">
            <dt>Folder</dt>
            <dd>{p.path || "(repository root)"}</dd>
            <dt>Engine</dt>
            <dd>
              {engineName(p.engine)} {p.engineVersion}
            </dd>
            <dt>Repository</dt>
            <dd>{p.repoUrl ? `${p.repoUrl}${p.branch ? ` @ ${p.branch}` : ""}` : "None yet"}</dd>
            <dt>Created</dt>
            <dd>{ws.data?.workspace?.createdAt ? timestampDate(ws.data.workspace.createdAt).toLocaleDateString() : ""}</dd>
          </dl>
        </section>
        <section className="card" aria-labelledby="ws-h">
          <h2 id="ws-h">Workspaces in {project}</h2>
          <ul className="rows">
            {p.workspaces.map((w) => (
              <li key={w.name}>
                <Link to="/$org/projects/$project/$workspace" params={{ org, project, workspace: w.name }}>
                  <span className="mono">{w.name}</span>
                  {w.name === workspace && <span className="muted">This one</span>}
                </Link>
              </li>
            ))}
          </ul>
        </section>
      </div>

      {admin && (
        <section className="card stack" aria-labelledby="settings-h">
          <h2 id="settings-h">Project settings</h2>
          <form className="stack" style={{ maxWidth: 560 }} onSubmit={save} key={`${p.path}|${p.engineVersion}|${p.repoUrl}|${p.branch}`}>
            <div className="row">
              <label className="field grow">
                <span>Folder</span>
                <input name="path" className="mono" defaultValue={p.path} />
              </label>
              <label className="field">
                <span>{engineName(p.engine)} version</span>
                <input name="version" className="mono" required defaultValue={p.engineVersion} />
              </label>
            </div>
            <div className="row">
              <label className="field grow">
                <span>Repository URL</span>
                <input name="repoUrl" type="url" defaultValue={p.repoUrl} />
              </label>
              <label className="field">
                <span>Branch</span>
                <input name="branch" defaultValue={p.branch} />
              </label>
            </div>
            <div className="row">
              <button type="submit">Save</button>
            </div>
          </form>
          {p.layout === Layout.CLI_WORKSPACES && (
            <form className="row" onSubmit={addWorkspace}>
              <label className="field">
                <span>New workspace</span>
                <input name="name" required className="mono" pattern="[a-z0-9]([a-z0-9_\-]{0,38}[a-z0-9])?" />
              </label>
              <button type="submit">Add workspace</button>
            </form>
          )}
          <div className="row">
            {p.workspaces.length > 1 && (
              <button
                type="button"
                className="danger"
                onClick={() =>
                  window.confirm(`Archive the workspace ${workspace}? It leaves every list.`) &&
                  act(() => projectApi.archiveWorkspace({ org, project, workspace }), () => navigate({ to: "/$org", params: { org } }))
                }
              >
                Archive workspace {workspace}
              </button>
            )}
            <button
              type="button"
              className="danger"
              onClick={() =>
                window.confirm(`Archive the project ${project} and all its workspaces?`) &&
                act(() => projectApi.archiveProject({ org, project }), () => navigate({ to: "/$org", params: { org } }))
              }
            >
              Archive project {project}
            </button>
          </div>
        </section>
      )}
    </div>
  );
}

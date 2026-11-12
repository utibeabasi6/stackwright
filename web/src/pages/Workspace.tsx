import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../Action";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { message, projectApi } from "../api";
import { describe, engineName } from "../engines";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { Layout } from "../gen/stackwright/v1/projects_pb";
import { EngineVersion } from "../EngineVersion";
import { useShell } from "../me";
import { formatLabels, parseLabels } from "../labels";
import { useLoad } from "../useLoad";
import { CloudPanel } from "./CloudPanel";
import { RunsPanel } from "./RunsPanel";
import { Variables } from "./Variables";
import { StatePanel } from "./StatePanel";

const titles = {
  overview: "Overview",
  state: "State",
  variables: "Variables",
  cloud: "Cloud credentials",
  settings: "Settings",
} as const;

export type Section = keyof typeof titles;

// Workspace shows one of a workspace's pages; the sidebar links them.
export function Workspace({ section }: { section: Section }) {
  const { org, project, workspace } = useParams({ strict: false }) as { org: string; project: string; workspace: string };
  const { me, reloadProjects } = useShell();
  const navigate = useNavigate();
  const ws = useLoad(`${org}/${project}/${workspace}`, () => projectApi.getWorkspace({ org, project, workspace }));
  const [error, setError] = useState<string>();
  const [version, setVersion] = useState("");
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;
  const savingProject = useAction();
  const savingLabels = useAction();
  const adding = useAction();

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
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const save = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    void savingProject.run(() =>
      act(() =>
      projectApi.updateProject({
        org,
        project,
        path: String(f.get("path")),
        engineVersion: version || p.engineVersion,
        repoUrl: String(f.get("repoUrl")),
        branch: String(f.get("branch")),
      }),
      ),
    );
  };
  const saveLabels = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const values = parseLabels(String(new FormData(e.currentTarget).get("labels")));
    void savingLabels.run(() => act(() => projectApi.updateWorkspace({ org, project, workspace, requiredLabels: { values } })));
  };
  const labels = formatLabels(ws.data?.workspace?.requiredLabels ?? {});
  const addWorkspace = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const name = String(new FormData(form).get("name"));
    void adding.run(() => act(() => projectApi.createWorkspace({ org, project, name }), () => form.reset()));
  };

  return (
    <div className="stack">
      <div>
        <h1 className={section === "overview" ? "mono" : undefined}>{section === "overview" ? workspace : titles[section]}</h1>
        {section === "overview" && (
          <span className="muted">
            {project} · {describe(p)}
          </span>
        )}
      </div>

      <div className="stack">
        {section === "overview" && (
          <RunsPanel
            org={org}
            project={project}
            workspace={workspace}
            sample={p.sample}
            linkedRepo={p.linkedRepo}
            runBranch={p.runBranch}
            autoApply={ws.data?.workspace?.autoApply ?? false}
            requiredLabels={ws.data?.workspace?.requiredLabels ?? {}}
          />
        )}

        {section === "state" && <StatePanel org={org} project={project} workspace={workspace} engine={p.engine} admin={admin} />}

        {section === "variables" && <Variables org={org} project={project} workspace={workspace} admin={admin} />}

        {section === "cloud" && (
          <CloudPanel
            org={org}
            project={project}
            workspace={workspace}
            account={ws.data?.workspace?.cloudAccount ?? ""}
            admin={admin}
            reload={ws.reload}
          />
        )}

        {section === "settings" && (
          <>
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
                  <dd>
                    {p.linkedRepo
                      ? `${p.linkedRepo} @ ${p.runBranch}, linked`
                      : p.repoUrl
                        ? `${p.repoUrl}${p.branch ? ` @ ${p.branch}` : ""}, not covered by a linked GitHub installation`
                        : "None yet"}
                  </dd>
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
              <section className="card stack" aria-labelledby="ws-settings-h">
                <h2 id="ws-settings-h">Runs in {workspace}</h2>
                <label className="check">
                  <input
                    type="checkbox"
                    checked={ws.data?.workspace?.autoApply ?? false}
                    onChange={(e) => act(() => projectApi.updateWorkspace({ org, project, workspace, autoApply: e.target.checked }))}
                  />{" "}
                  Auto-apply plans: a plan with changes applies with no approval
                </label>
                <form className="row" onSubmit={saveLabels} key={labels}>
                  <label className="field grow">
                    <span>Build server labels this workspace requires</span>
                    <input name="labels" defaultValue={labels} placeholder="region=eu" className="mono" />
                  </label>
                  <Submit action={savingLabels} busy="Saving…" done="Saved">
                    Save labels
                  </Submit>
                </form>
                <p className="hint">Runs go only to build servers with all of these labels. Leave it empty for any build server.</p>
              </section>
            )}

            {admin && (
              <section className="card stack" aria-labelledby="settings-h">
                <h2 id="settings-h">Project settings</h2>
                <form className="stack" style={{ maxWidth: 560 }} onSubmit={save} key={`${p.path}|${p.engineVersion}|${p.repoUrl}|${p.branch}`}>
                  <div className="row">
                    <label className="field grow">
                      <span>Folder</span>
                      <input name="path" className="mono" defaultValue={p.path} />
                    </label>
                    <EngineVersion engine={p.engine} value={version || p.engineVersion} onChange={setVersion} label={`${engineName(p.engine)} version`} />
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
                    <Submit action={savingProject} busy="Saving…" done="Saved">
                      Save
                    </Submit>
                  </div>
                </form>
                {p.layout === Layout.CLI_WORKSPACES && (
                  <form className="row" onSubmit={addWorkspace}>
                    <label className="field">
                      <span>New workspace</span>
                      <input name="name" required className="mono" pattern="[a-z0-9]([a-z0-9_\-]{0,38}[a-z0-9])?" />
                    </label>
                    <Submit action={adding} busy="Adding…" done="Added">
                      Add workspace
                    </Submit>
                  </form>
                )}
                <div className="row">
                  {p.workspaces.length > 1 && (
                    <Action
                      className="danger"
                      run={() =>
                        window.confirm(`Archive the workspace ${workspace}? It leaves every list.`) &&
                        act(() => projectApi.archiveWorkspace({ org, project, workspace }), () => navigate({ to: "/$org", params: { org } }))
                      }
                      busy="Archiving…"
                      done="Archived"
                    >
                      Archive workspace {workspace}
                    </Action>
                  )}
                  <Action
                    className="danger"
                    run={() =>
                      window.confirm(`Archive the project ${project} and all its workspaces?`) &&
                      act(() => projectApi.archiveProject({ org, project }), () => navigate({ to: "/$org", params: { org } }))
                    }
                    busy="Archiving…"
                    done="Archived"
                  >
                    Archive project {project}
                  </Action>
                </div>
              </section>
            )}
          </>
        )}
      </div>
    </div>
  );
}

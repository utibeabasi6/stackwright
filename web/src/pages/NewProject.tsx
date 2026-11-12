import { useState, type FormEvent } from "react";
import { Submit, useAction } from "../Action";
import { useNavigate, useParams } from "@tanstack/react-router";
import { cloudApi, githubApi, message, projectApi } from "../api";
import { Combobox } from "../Combobox";
import { EngineVersion } from "../EngineVersion";
import { Select } from "../Select";
import { useLoad } from "../useLoad";
import { Engine, Layout } from "../gen/stackwright/v1/projects_pb";
import { useShell } from "../me";

export function NewProject() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { reloadProjects } = useShell();
  const navigate = useNavigate();
  const [engine, setEngine] = useState(Engine.OPENTOFU);
  const [version, setVersion] = useState("");
  const [layout, setLayout] = useState(Layout.CLI_WORKSPACES);
  const [error, setError] = useState<string>();
  const pending = useAction();
  const repos = useLoad(`github:${org}`, () => githubApi.getGitHub({ org }));
  const accounts = useLoad(`cloud-accounts:${org}`, () => cloudApi.listCloudAccounts({ org }).then((r) => r.accounts));

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    try {
      const r = await projectApi.createProject({
        org,
        slug: String(f.get("slug")),
        path: String(f.get("path")),
        engine,
        engineVersion: version,
        layout,
        workspaces: String(f.get("workspaces") ?? "")
          .split(/[\s,]+/)
          .filter(Boolean),
        repoUrl: String(f.get("repoUrl")),
        branch: String(f.get("branch")),
        cloudAccount: String(f.get("cloudAccount") ?? ""),
      });
      reloadProjects();
      const p = r.project!;
      navigate({ to: "/$org/projects/$project/$workspace", params: { org, project: p.slug, workspace: p.workspaces[0].name } });
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  return (
    <>
      <h1>New project</h1>
      <form className="stack" style={{ maxWidth: 560 }} onSubmit={(e) => void pending.run(() => submit(e))}>
        <label className="field">
          <span>Short name</span>
          <input name="slug" required className="mono" pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?" placeholder="network" />
        </label>
        <label className="field">
          <span>Folder in the repository</span>
          <input name="path" className="mono" placeholder="network/" />
          <span className="hint">Leave it empty for the repository's root.</span>
        </label>
        <div className="row">
          <label className="field">
            <span>Engine</span>
            <Select
              value={String(engine)}
              onChange={(v) => {
                setEngine(Number(v) as Engine);
                setVersion("");
              }}
              options={[
                { value: String(Engine.OPENTOFU), label: "OpenTofu" },
                { value: String(Engine.TERRAFORM), label: "Terraform" },
              ]}
            />
          </label>
          <EngineVersion engine={engine} value={version} onChange={setVersion} />
        </div>
        <fieldset className="field">
          <legend>Layout</legend>
          <label>
            <input type="radio" checked={layout === Layout.CLI_WORKSPACES} onChange={() => setLayout(Layout.CLI_WORKSPACES)} />{" "}
            CLI workspaces: one state for each <code>tofu workspace</code>
          </label>
          <label>
            <input type="radio" checked={layout === Layout.PLAIN} onChange={() => setLayout(Layout.PLAIN)} /> Plain folder: one
            state, named default
          </label>
        </fieldset>
        {layout === Layout.CLI_WORKSPACES && (
          <label className="field">
            <span>Workspaces</span>
            <input name="workspaces" required className="mono" defaultValue="dev, staging, prod" />
            <span className="hint">Separate them with commas or spaces.</span>
          </label>
        )}
        <div className="row">
          <label className="field grow">
            <span>Repository URL (optional)</span>
            <Combobox
              name="repoUrl"
              type="url"
              placeholder="https://github.com/acme/infra"
              options={(repos.data?.installations ?? []).flatMap((i) =>
                i.repos.map((r) => ({ value: `https://github.com/${r.fullName}`, label: r.fullName })),
              )}
            />
            <span className="hint">A repository that the org's GitHub installation covers plans pull requests and applies merges.</span>
          </label>
          <label className="field">
            <span>Branch</span>
            <input name="branch" placeholder="main" />
          </label>
        </div>
        {(accounts.data?.length ?? 0) > 0 && (
          <label className="field">
            <span>Cloud account</span>
            <Select
              name="cloudAccount"
              defaultValue=""
              options={[
                { value: "", label: "None" },
                ...(accounts.data ?? []).map((a) => ({ value: a.name, label: `${a.name} (AWS ${a.aws?.region})`, text: a.name })),
              ]}
            />
            <span className="hint">Every workspace of the project starts with it. Each workspace can change it later.</span>
          </label>
        )}
        {error && <p className="error">{error}</p>}
        <div className="row">
          <Submit action={pending} className="primary" busy="Creating…" done="Created">
            Create project
          </Submit>
          <button type="button" onClick={() => navigate({ to: "/$org", params: { org } })}>
            Cancel
          </button>
        </div>
      </form>
    </>
  );
}

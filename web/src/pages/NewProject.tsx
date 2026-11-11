import { useState, type FormEvent } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { message, projectApi } from "../api";
import { Engine, Layout } from "../gen/stackwright/v1/projects_pb";
import { useShell } from "../me";

const defaults = { [Engine.OPENTOFU]: "1.10.7", [Engine.TERRAFORM]: "1.5.7" } as Record<Engine, string>;

export function NewProject() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { reloadProjects } = useShell();
  const navigate = useNavigate();
  const [engine, setEngine] = useState(Engine.OPENTOFU);
  const [version, setVersion] = useState(defaults[Engine.OPENTOFU]);
  const [layout, setLayout] = useState(Layout.CLI_WORKSPACES);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setBusy(true);
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
      });
      reloadProjects();
      const p = r.project!;
      navigate({ to: "/$org/projects/$project/$workspace", params: { org, project: p.slug, workspace: p.workspaces[0].name } });
    } catch (err) {
      setError(message(err));
      setBusy(false);
    }
  };

  return (
    <>
      <h1>New project</h1>
      <form className="stack" style={{ maxWidth: 560 }} onSubmit={submit}>
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
            <select
              value={engine}
              onChange={(e) => {
                const next = Number(e.target.value) as Engine;
                setEngine(next);
                setVersion(defaults[next]);
              }}
            >
              <option value={Engine.OPENTOFU}>OpenTofu</option>
              <option value={Engine.TERRAFORM}>Terraform</option>
            </select>
          </label>
          <label className="field">
            <span>Version</span>
            <input className="mono" required value={version} onChange={(e) => setVersion(e.target.value)} />
          </label>
        </div>
        <span className="hint">
          {engine === Engine.TERRAFORM
            ? "Terraform 1.0.0 to 1.5.7. Newer Terraform has a licence that does not allow it here."
            : "OpenTofu 1.8.0 or newer."}
        </span>
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
            <input name="repoUrl" type="url" placeholder="https://github.com/acme/infra" />
          </label>
          <label className="field">
            <span>Branch</span>
            <input name="branch" placeholder="main" />
          </label>
        </div>
        {error && <p className="error">{error}</p>}
        <div className="row">
          <button type="submit" className="primary" disabled={busy}>
            Create project
          </button>
          <button type="button" onClick={() => navigate({ to: "/$org", params: { org } })}>
            Cancel
          </button>
        </div>
      </form>
    </>
  );
}

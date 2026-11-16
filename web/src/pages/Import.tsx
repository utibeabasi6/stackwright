import { useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { cloudApi, githubApi, importApi, message } from "../api";
import { Action, Submit, useAction } from "../Action";
import { EngineVersion } from "../EngineVersion";
import { Engine, StateBackend } from "../gen/stackwright/v1/projects_pb";
import type { FoundRoot, ScanRepositoryResponse } from "../gen/stackwright/v1/imports_pb";
import { formatLabels, parseLabels } from "../labels";
import { Select } from "../Select";
import { useLoad } from "../useLoad";

type Choice = { chosen: boolean; slug: string; engine: Engine; version: string; labels: string };

// where says where a root's state lives and what that means for its runs
// (adr/0036).
function where(r: FoundRoot) {
  if (r.stateBackend === StateBackend.STACKWRIGHT && r.stackError) {
    return `A Terraform Stack whose files do not parse, so it cannot import yet: ${r.stackError}`;
  }
  if (r.stateBackend === StateBackend.STACKWRIGHT) {
    return `A Terraform Stack with ${r.stackDeployments.length === 1 ? "deployment" : "deployments"} ${r.stackDeployments.join(", ")}: each component of each deployment gets a workspace, and each deployment deploys them in order with one approval. Stackwright keeps their state.`;
  }
  if (r.units.length > 0) {
    return `A Terragrunt stack of ${r.units.length} ${r.units.length === 1 ? "unit" : "units"}: runs plan them in dependency order, on the backends their remote_state names.`;
  }
  if (r.stateBackend === StateBackend.CODE) return `Runs use its ${r.backend} backend, as the code says.`;
  if (r.committedState) return "Its state is committed to Git: runs plan from it and never apply.";
  return "It has a local backend and no state in the repository: runs only plan, and the first plan shows everything as new.";
}

export function Import() {
  const { org } = useParams({ strict: false }) as { org: string };
  const navigate = useNavigate();
  const gh = useLoad(`github:${org}`, () => githubApi.getGitHub({ org }));
  const accounts = useLoad(`cloud-accounts:${org}`, () => cloudApi.listCloudAccounts({ org }).then((r) => r.accounts));
  const repos = (gh.data?.installations ?? []).flatMap((i) => i.repos.map((r) => r.fullName));
  const [repo, setRepo] = useState("");
  const chosenRepo = useRef("");
  const [scan, setScan] = useState<ScanRepositoryResponse>();
  const [choices, setChoices] = useState<Record<string, Choice>>({});
  const [account, setAccount] = useState("");
  const [error, setError] = useState<string>();
  const importing = useAction();

  const runScan = async () => {
    setError(undefined);
    try {
      const r = await importApi.scanRepository({ org, repo });
      if (chosenRepo.current !== repo) return false;
      setScan(r);
      setChoices(
        Object.fromEntries(
          r.roots.map((root) => [
            root.path,
            { chosen: !root.existingProject, slug: root.slug, engine: root.engine, version: "", labels: formatLabels(root.labels) },
          ]),
        ),
      );
      return true;
    } catch (e) {
      if (chosenRepo.current !== repo) return false;
      setError(message(e));
      return false;
    }
  };
  const set = (path: string, change: Partial<Choice>) => setChoices((c) => ({ ...c, [path]: { ...c[path], ...change } }));

  const start = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!scan) return false;
    setError(undefined);
    try {
      const roots = scan.roots
        .filter((r) => choices[r.path]?.chosen)
        .map((r) => {
          const c = choices[r.path];
          return { path: r.path, slug: c.slug, engine: c.engine, engineVersion: c.version, labels: parseLabels(c.labels), cloudAccount: account };
        });
      const { import: imp } = await importApi.startImport({ org, repo, commit: scan.commit, roots });
      await navigate({ to: "/$org/imports/$id", params: { org, id: imp?.id ?? "" } });
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  if (gh.data && repos.length === 0) {
    return (
      <div className="stack">
        <h1>Import a repository</h1>
        <p className="notice">
          The importer reads repositories that a linked GitHub installation covers.{" "}
          <Link to="/$org/settings/github" params={{ org }}>
            Link one
          </Link>
          .
        </p>
      </div>
    );
  }
  const chosen = scan?.roots.filter((r) => choices[r.path]?.chosen).length ?? 0;
  return (
    <div className="stack">
      <h1>Import a repository</h1>
      <p className="muted">
        The importer finds each folder that OpenTofu or Terraform runs in, and makes it a project. Runs use the backend that the code names,
        so the repository keeps working as it does today, with or without Stackwright. Nothing in the repository or its state changes.
      </p>
      <div className="row" style={{ alignItems: "end" }}>
        <label className="field grow">
          <span>Repository</span>
          <Select
            aria-label="Repository"
            value={repo}
            onChange={(v) => {
              chosenRepo.current = v;
              setRepo(v);
              setScan(undefined);
            }}
            options={[{ value: "", label: "Choose a repository" }, ...repos.map((r) => ({ value: r, label: r }))]}
          />
        </label>
        <Action className="primary" disabled={!repo} run={runScan} busy="Scanning…" done="Scanned">
          Scan
        </Action>
      </div>
      {error && <p className="error">{error}</p>}
      {scan && (
        <form className="stack" onSubmit={(e) => void importing.run(() => start(e))} aria-label="Folders to import">
          <p className="muted mono">
            {repo} at {scan.commit.slice(0, 7)}
          </p>
          {scan.roots.length === 0 && <p className="notice">No folder here runs on its own.</p>}
          {scan.roots.map((r) => {
            const c = choices[r.path];
            return (
              <section key={r.path} className="card stack" aria-label={`Folder ${r.path || "(top)"}`}>
                <label className="check">
                  <input type="checkbox" checked={c.chosen} onChange={(e) => set(r.path, { chosen: e.target.checked })} />{" "}
                  <span className="mono">{r.path || "(the repository's top folder)"}</span>
                  {r.existingProject && <span className="pill">Imported as {r.existingProject}</span>}
                </label>
                <p className="hint">
                  {where(r)}
                  {r.workspaces.length > 0 && (
                    <>
                      {" "}
                      Workspaces: <span className="mono">{r.workspaces.join(", ")}</span>.
                    </>
                  )}
                </p>
                {c.chosen && !r.existingProject && (
                  <div className="row">
                    <label className="field">
                      <span>Project name</span>
                      <input className="mono" required value={c.slug} onChange={(e) => set(r.path, { slug: e.target.value })} />
                    </label>
                    <label className="field">
                      <span>Engine</span>
                      <Select
                        aria-label="Engine"
                        value={String(c.engine)}
                        onChange={(v) => set(r.path, { engine: Number(v) as Engine, version: "" })}
                        options={[
                          { value: String(Engine.OPENTOFU), label: "OpenTofu" },
                          { value: String(Engine.TERRAFORM), label: "Terraform" },
                        ]}
                      />
                    </label>
                    <EngineVersion engine={c.engine} value={c.version} onChange={(v) => set(r.path, { version: v })} />
                    <label className="field grow">
                      <span>Labels</span>
                      <input className="mono" value={c.labels} onChange={(e) => set(r.path, { labels: e.target.value })} />
                    </label>
                  </div>
                )}
              </section>
            );
          })}
          {scan.modules.length > 0 && (
            <section className="card stack" aria-label="Not imported">
              <h2>Not imported</h2>
              <p className="hint">
                Modules that the folders above call: <span className="mono">{scan.modules.join(", ")}</span>.
              </p>
            </section>
          )}
          {(accounts.data?.length ?? 0) > 0 && (
            <label className="field" style={{ maxWidth: 360 }}>
              <span>Cloud account for the new workspaces</span>
              <Select
                aria-label="Cloud account"
                value={account}
                onChange={setAccount}
                options={[{ value: "", label: "None" }, ...(accounts.data ?? []).map((a) => ({ value: a.name, label: a.name }))]}
              />
            </label>
          )}
          <p className="hint">Each new workspace gets a plan at once. A plan with no changes shows the import found the right state.</p>
          <div className="row">
            <Submit action={importing} className="primary" disabled={chosen === 0} busy="Importing…" done="Imported">
              Import {chosen} {chosen === 1 ? "folder" : "folders"}
            </Submit>
          </div>
        </form>
      )}
    </div>
  );
}

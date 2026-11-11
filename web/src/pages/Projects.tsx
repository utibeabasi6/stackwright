import { useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { message, projectApi } from "../api";
import { describe } from "../engines";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useShell } from "../me";

export function Projects() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { me, projects, reloadProjects } = useShell();
  const navigate = useNavigate();
  const [error, setError] = useState<string>();
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;

  const trySample = async () => {
    try {
      const r = await projectApi.createSampleProject({ org });
      reloadProjects();
      const ws = r.project?.workspaces[0]?.name ?? "dev";
      navigate({ to: "/$org/projects/$project/$workspace", params: { org, project: r.project?.slug ?? "sample", workspace: ws } });
    } catch (err) {
      setError(message(err));
    }
  };

  if (!projects) return null;
  return (
    <div className="stack">
      <div className="row" style={{ alignItems: "center" }}>
        <h1 className="grow" style={{ margin: 0 }}>
          Projects
        </h1>
        {admin && projects.length > 0 && (
          <Link className="button primary" to="/$org/projects/new" params={{ org }}>
            New project
          </Link>
        )}
      </div>
      {error && <p className="error">{error}</p>}
      {projects.length === 0 ? (
        <div className="card stack empty">
          <h2>No projects yet</h2>
          {admin ? (
            <>
              <p className="muted">
                A project is a folder of OpenTofu or Terraform code. Each of its workspaces holds one state. Start with the
                sample, which needs no cloud account, or add your own.
              </p>
              <div className="row">
                <button type="button" className="primary" onClick={trySample}>
                  Try the sample
                </button>
                <Link className="button" to="/$org/projects/new" params={{ org }}>
                  New project
                </Link>
              </div>
            </>
          ) : (
            <p className="muted">An admin of this org can add one.</p>
          )}
        </div>
      ) : (
        <div className="cards">
          {projects.map((p) => (
            <section key={p.slug} className="card stack" aria-label={`Project ${p.slug}`}>
              <div>
                <h2 className="mono" style={{ margin: 0 }}>
                  {p.slug}
                  {p.sample && <span className="pill">Sample</span>}
                </h2>
                <span className="hint">{describe(p)}</span>
              </div>
              <ul className="rows">
                {p.workspaces.map((w) => (
                  <li key={w.name}>
                    <Link to="/$org/projects/$project/$workspace" params={{ org, project: p.slug, workspace: w.name }}>
                      <span className="mono">{w.name}</span>
                      <span className="muted">No runs yet</span>
                    </Link>
                  </li>
                ))}
              </ul>
              {p.repoUrl && <span className="hint mono">{p.repoUrl}</span>}
            </section>
          ))}
        </div>
      )}
    </div>
  );
}

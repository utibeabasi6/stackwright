import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { message, projectApi } from "../api";
import { describe } from "../engines";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useRole, useShell } from "../me";
import type { Project } from "../gen/stackwright/v1/projects_pb";

const groupKey = "stackwright.projects.groupBy";

type Node = { name: string; projects: Project[]; children: Node[] };

// groups nests projects by the label keys in order (adr/0035). A path
// label splits into its folders; a project without a key goes under
// "No <key>".
function groups(projects: Project[], by: string[]): Node {
  const root: Node = { name: "", projects: [], children: [] };
  for (const p of projects) {
    const trail = by.flatMap((k) => {
      const v = p.labels[k];
      if (!v) return [`No ${k}`];
      return k === "path" ? v.split("/").slice(0, -1) : [`${k}=${v}`];
    });
    let n = root;
    for (const name of trail) {
      let child = n.children.find((c) => c.name === name);
      if (!child) n.children.push((child = { name, projects: [], children: [] }));
      n = child;
    }
    n.projects.push(p);
  }
  const sort = (n: Node) => {
    n.children.sort((a, b) => a.name.localeCompare(b.name));
    n.children.forEach(sort);
  };
  sort(root);
  return root;
}

function savedGroupBy(): string[] {
  try {
    return JSON.parse(window.localStorage.getItem(groupKey) ?? "[]") as string[];
  } catch {
    return [];
  }
}

export function Projects() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { projects, reloadProjects } = useShell();
  const navigate = useNavigate();
  const [error, setError] = useState<string>();
  const admin = useRole(org) >= Role.ADMIN;
  const [groupBy, setGroupBy] = useState(savedGroupBy);
  const keys = useMemo(() => [...new Set((projects ?? []).flatMap((p) => Object.keys(p.labels)))].sort(), [projects]);
  const tree = useMemo(() => groups(projects ?? [], groupBy.filter((k) => keys.includes(k))), [projects, groupBy, keys]);
  const toggle = (k: string) => {
    const next = groupBy.includes(k) ? groupBy.filter((x) => x !== k) : [...groupBy, k];
    setGroupBy(next);
    try {
      window.localStorage.setItem(groupKey, JSON.stringify(next));
    } catch {
      // Storage can be refused; the grouping then lasts until reload.
    }
  };

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
          <>
            <Link className="button" to="/$org/import" params={{ org }}>
              Import a repository
            </Link>
            <Link className="button primary" to="/$org/projects/new" params={{ org }}>
              New project
            </Link>
          </>
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
                <Link className="button" to="/$org/import" params={{ org }}>
                  Import a repository
                </Link>
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
        <>
          {keys.length > 0 && (
            <div className="row group-by" role="group" aria-label="Group by">
              <span className="muted">Group by</span>
              {keys.map((k) => (
                <button key={k} type="button" aria-pressed={groupBy.includes(k)} onClick={() => toggle(k)}>
                  {k}
                  {groupBy.includes(k) && groupBy.length > 1 && <span className="muted"> {groupBy.indexOf(k) + 1}</span>}
                </button>
              ))}
            </div>
          )}
          <Group node={tree} depth={2} org={org} />
        </>
      )}
    </div>
  );
}

function Group({ node, depth, org }: { node: Node; depth: number; org: string }) {
  const Heading = `h${Math.min(depth, 6)}` as "h2";
  return (
    <>
      {node.projects.length > 0 && (
        <div className="cards">
          {node.projects.map((p) => (
            <ProjectCard key={p.slug} p={p} org={org} />
          ))}
        </div>
      )}
      {node.children.map((c) => (
        <section key={c.name} className="project-group stack" aria-label={c.name}>
          <Heading className="mono">{c.name}</Heading>
          <Group node={c} depth={depth + 1} org={org} />
        </section>
      ))}
    </>
  );
}

function ProjectCard({ p, org }: { p: Project; org: string }) {
  const labels = Object.entries(p.labels).sort(([a], [b]) => a.localeCompare(b));
  return (
    <section className="card stack" aria-label={`Project ${p.slug}`}>
      <div>
        <h2 className="mono" style={{ margin: 0 }}>
          {p.slug}
          {p.sample && <span className="pill">Sample</span>}
        </h2>
        <span className="hint">{describe(p)}</span>
      </div>
      {labels.length > 0 && (
        <ul className="labels" aria-label="Labels">
          {labels.map(([k, v]) => (
            <li key={k} className="mono">
              {k}={v}
            </li>
          ))}
        </ul>
      )}
      <ul className="rows">
        {p.workspaces.map((w) => (
          <li key={w.name}>
            <Link to="/$org/projects/$project/$workspace" params={{ org, project: p.slug, workspace: w.name }}>
              <span className="mono">{w.name}</span>
            </Link>
          </li>
        ))}
      </ul>
      {p.repoUrl && <span className="hint mono">{p.repoUrl}</span>}
      <Link to="/$org/projects/$project/topology" params={{ org, project: p.slug }}>
        Topology
      </Link>
    </section>
  );
}

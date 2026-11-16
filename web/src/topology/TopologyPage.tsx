import { useMemo, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { graphApi } from "../api";
import { PlanAction, TopologyRole, type Topology, type TopologyNode } from "../gen/stackwright/v1/graph_pb";
import { severities } from "../pages/Findings";
import { actionInfo } from "../plan/actions";
import { useLoad } from "../useLoad";
import { layout } from "./layout";

const roleNames: Record<TopologyRole, string> = {
  [TopologyRole.UNSPECIFIED]: "Resource",
  [TopologyRole.VPC]: "VPC",
  [TopologyRole.SUBNET]: "Subnet",
  [TopologyRole.INTERNET_GATEWAY]: "Internet gateway",
  [TopologyRole.NAT_GATEWAY]: "NAT gateway",
  [TopologyRole.ROUTE_TABLE]: "Route table",
  [TopologyRole.SECURITY_GROUP]: "Security group",
  [TopologyRole.LOAD_BALANCER]: "Load balancer",
  [TopologyRole.INSTANCE_GROUP]: "Instance group",
  [TopologyRole.INSTANCE]: "Instance",
  [TopologyRole.DATABASE]: "Database",
  [TopologyRole.CLUSTER]: "Cluster",
  [TopologyRole.PEERING]: "Peering",
  [TopologyRole.EXTERNAL]: "Not in any plan",
  [TopologyRole.INTERNET]: "Internet",
};

const actionClass = (a: PlanAction) => (actionInfo[a].tone === "muted" ? undefined : actionInfo[a].tone);

const actionText: Partial<Record<PlanAction, { glyph: string; text: string }>> = {
  [PlanAction.CREATE]: { glyph: "+", text: "created by the plan" },
  [PlanAction.UPDATE]: { glyph: "~", text: "updated by the plan" },
  [PlanAction.REPLACE]: { glyph: "±", text: "replaced by the plan" },
  [PlanAction.REPLACE_CREATE_FIRST]: { glyph: "±", text: "replaced by the plan" },
  [PlanAction.DELETE]: { glyph: "−", text: "destroyed by the plan" },
};

// TopologyPage draws a project's AWS network from each workspace's latest
// plan, with what it uses from other projects dimmed (adr/0039). A node
// opens its details in the side panel.
export function TopologyPage() {
  const { org, project } = useParams({ strict: false }) as { org: string; project: string };
  const res = useLoad(`topology:${org}/${project}`, () => graphApi.getTopology({ org, project }));
  const [selected, setSelected] = useState<string>();
  const topo = res.data?.topology;
  const l = useMemo(() => (topo ? layout(topo) : undefined), [topo]);

  if (res.error) {
    return (
      <div className="stack">
        <h1>Topology</h1>
        <p className="error">{res.error}</p>
      </div>
    );
  }
  if (!topo || !l) return null;
  const nodes = new Map(topo.nodes.map((n) => [n.id, n]));
  const chosen = selected ? nodes.get(selected) : undefined;
  const depth = (n: TopologyNode) => (n.role === TopologyRole.VPC ? 0 : n.role === TopologyRole.SUBNET ? 1 : 2);
  const drawn = topo.nodes.filter((n) => l.boxes.has(n.id)).sort((a, b) => depth(a) - depth(b));

  return (
    <div className="topology-page">
      <div className="topo-canvas" aria-label="Topology">
        {drawn.length === 0 ? (
          <p className="notice muted topo-empty">
            No plan of {project} has AWS network resources yet. The topology reads each workspace's latest plan of its own code.
          </p>
        ) : (
          <div className="topo-stage" style={{ width: l.width, height: l.height }}>
            <svg className="topo-edges" width={l.width} height={l.height} aria-hidden="true">
              {l.paths.map(({ edge, d }) => (
                <path key={`${edge.from}>${edge.to}>${edge.label}`} d={d} className={`topo-edge ${actionClass(edge.action) ?? ""}`} />
              ))}
            </svg>
            {drawn.map((n) => {
              const b = l.boxes.get(n.id)!;
              return (
                <button
                  key={n.id}
                  type="button"
                  className={["topo-node", `role-${TopologyRole[n.role].toLowerCase()}`, actionClass(n.action), n.public ? "public" : "",
                    n.context ? "context" : "", selected === n.id ? "selected" : ""].filter(Boolean).join(" ")}
                  style={{ left: b.x, top: b.y, width: b.w, height: b.h }}
                  onClick={() => setSelected(n.id)}
                  aria-pressed={selected === n.id}
                  aria-label={[
                    `${roleNames[n.role]} ${n.name}`,
                    n.context && `from ${n.project}`,
                    actionText[n.action]?.text,
                    n.findings > 0 && `${n.findings} open ${n.findings === 1 ? "finding" : "findings"}`,
                  ]
                    .filter(Boolean)
                    .join(", ")}
                >
                  <span className="topo-role">
                    {n.context && !(n.vpc && nodes.get(n.vpc)?.context) && `${n.project} · `}
                    {n.role === TopologyRole.SUBNET ? (n.public ? "Public" : "Private") : roleNames[n.role]}
                  </span>
                  <span className="topo-name">{n.name}</span>
                  {n.cidr && <span className="topo-cidr mono">{n.cidr}</span>}
                  {n.findings > 0 && (
                    <span className="topo-findings error" title={`${n.findings} open findings`}>
                      ! {n.findings}
                    </span>
                  )}
                </button>
              );
            })}
            {l.columns.map((c) => (
              <span key={`${c.vpc}/${c.zone}`} className="topo-zone hint" style={{ left: c.x, top: c.y, width: c.w }}>
                {c.zone || "Zone not known"}
              </span>
            ))}
          </div>
        )}
      </div>
      <aside className="inspector" aria-label="Details">
        {chosen ? (
          <Details org={org} node={chosen} topo={topo} select={setSelected} />
        ) : (
          <div className="stack">
            <h2 className="mono">{project}</h2>
            <p className="muted">
              The latest plan of each workspace's own code. Dashed nodes are planned. Dimmed nodes belong to another project and are shown
              because this one uses them. Select a node for its details.
            </p>
            {topo.clashes.length > 0 && (
              <div className="notice">
                Two workspaces manage the same AWS ids. The first workspace's resource is shown.
                <ul>
                  {topo.clashes.map((c) => (
                    <li key={c} className="mono">
                      {c}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            <h3>Plans read</h3>
            <ul className="mono">
              {res.data?.sources.map((s) => <li key={s}>{s}</li>)}
            </ul>
          </div>
        )}
      </aside>
    </div>
  );
}

function Details({ org, node: n, topo, select }: { org: string; node: TopologyNode; topo: Topology; select: (id: string) => void }) {
  const byId = new Map(topo.nodes.map((x) => [x.id, x]));
  const label = (id: string) => byId.get(id)?.name ?? id;
  const go = (id: string) => (
    <button type="button" className="link mono" onClick={() => select(id)}>
      {label(id)}
    </button>
  );
  const members = topo.nodes.filter((m) => m.subnets.includes(n.id) || m.securityGroups.includes(n.id));
  return (
    <div className="stack">
      <div>
        <span className="hint">{roleNames[n.role]}</span>
        <h2>{n.name}</h2>
        {actionText[n.action] && (
          <p className={actionClass(n.action)}>
            {actionText[n.action]!.glyph} {actionText[n.action]!.text}
          </p>
        )}
        {n.context && (
          <p className="muted">
            From project{" "}
            <Link to="/$org/projects/$project/topology" params={{ org, project: n.project }} className="mono">
              {n.project}
            </Link>
            , shown because this project uses it.
          </p>
        )}
      </div>
      <dl className="facts">
        {n.project && (
          <>
            <dt>Workspace</dt>
            <dd>
              <Link to="/$org/projects/$project/$workspace" params={{ org, project: n.project, workspace: n.workspace }} className="mono">
                {n.project} / {n.workspace}
              </Link>
            </dd>
            <dt>Address</dt>
            <dd className="mono">{n.address}</dd>
          </>
        )}
        <dt>AWS id</dt>
        <dd className="mono">{n.awsId || <span className="muted">known after apply</span>}</dd>
        {n.cidr && (
          <>
            <dt>CIDR</dt>
            <dd className="mono">{n.cidr}</dd>
          </>
        )}
        {n.zone && (
          <>
            <dt>Zone</dt>
            <dd className="mono">{n.zone}</dd>
          </>
        )}
        {n.role === TopologyRole.SUBNET && (
          <>
            <dt>Reach</dt>
            <dd>{n.public ? "Public: 0.0.0.0/0 goes to an internet gateway" : "Private"}</dd>
          </>
        )}
        {n.findings > 0 && (
          <>
            <dt>Findings</dt>
            <dd className="error">
              {n.findings} open, the worst {severities[n.worst].text}.{" "}
              <Link to="/$org/projects/$project/$workspace/security" params={{ org, project: n.project, workspace: n.workspace }}>
                See them
              </Link>
            </dd>
          </>
        )}
      </dl>

      {n.routes.length > 0 && (
        <section className="stack">
          <h3>Routes</h3>
          <table className="table">
            <thead>
              <tr>
                <th>Destination</th>
                <th>Target</th>
              </tr>
            </thead>
            <tbody>
              {n.routes.map((r) => (
                <tr key={`${r.destination}>${r.target}`}>
                  <td className="mono">{r.destination}</td>
                  <td>{go(r.target)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      {n.rules.length > 0 && (
        <section className="stack">
          <h3>Rules</h3>
          <table className="table">
            <thead>
              <tr>
                <th>Direction</th>
                <th>Ports</th>
                <th>Source or destination</th>
              </tr>
            </thead>
            <tbody>
              {n.rules.map((r, i) => (
                <tr key={i}>
                  <td>{r.ingress ? "In" : "Out"}</td>
                  <td className="mono">
                    {r.protocol === "-1" ? "all" : `${r.protocol} ${r.fromPort === r.toPort ? r.fromPort : `${r.fromPort}-${r.toPort}`}`}
                  </td>
                  <td>
                    {r.self && <span>this group </span>}
                    {r.securityGroup && go(r.securityGroup)}
                    {r.cidrs.map((c) => (
                      <span key={c} className={`mono ${c === "0.0.0.0/0" || c === "::/0" ? "warn" : ""}`}>
                        {c}{" "}
                      </span>
                    ))}
                    {r.description && <span className="muted"> · {r.description}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      {members.length > 0 && (
        <section className="stack">
          <h3>{n.role === TopologyRole.SECURITY_GROUP ? "Attached to" : "In this subnet"}</h3>
          <ul>
            {members.map((m) => (
              <li key={m.id}>
                {roleNames[m.role]}: {go(m.id)}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

import { useEffect } from "react";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { graphApi } from "../api";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useShell } from "../me";
import { Graph } from "../plan/Graph";
import { useLoad } from "../useLoad";

export function Plan() {
  const { org, project, workspace } = useParams({ strict: false }) as { org: string; project: string; workspace: string };
  const { run } = useSearch({ strict: false }) as { run?: string };
  const number = Number(run ?? 0);
  const { me, openTab } = useShell();
  useEffect(() => openTab({ project, workspace }), [project, workspace, openTab]);
  const graph = useLoad(`plan-graph:${org}/${project}/${workspace}/${number}`, () =>
    graphApi.getPlanGraph({ org, project, workspace, number }),
  );
  const writer = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.WRITER;

  return (
    <div className="plan-view">
      {graph.error && (
        <div className="plan-empty">
          <p>{graph.error}</p>
          <Link to="/$org/projects/$project/$workspace" params={{ org, project, workspace }}>
            Start a plan from the Overview
          </Link>
        </div>
      )}
      {graph.data && (
        <Graph org={org} project={project} workspace={workspace} data={graph.data} writer={writer} />
      )}
    </div>
  );
}

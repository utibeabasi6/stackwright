import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Edge as FlowEdge,
  type Node as FlowNode,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/base.css";
import { graphApi } from "../api";
import { PlanAction, type GetPlanGraphResponse, type PlanGroup, type SourceLocation } from "../gen/stackwright/v1/graph_pb";
import { useLoad } from "../useLoad";
import { actionInfo, countsText, summaryText } from "./actions";
import type { Diagnostic, NodeStatus } from "./apply";
import { boxes, layout, nodeHeight, nodeWidth, place, visible, type Item, type Point, type View } from "./layout";

// Over this many nodes, a plan opens with its top-level modules closed.
const openLimit = 300;

type ItemData = { item: Item; match: boolean; status?: NodeStatus };

const statusWord = { waiting: "waiting", running: "in progress", done: "done", failed: "failed" } as const;
const statusMark = { waiting: "", running: "…", done: "✓", failed: "!" } as const;

function label(item: Item) {
  if (!item.node) return item.id;
  return item.node.module ? item.id.slice(item.node.module.length + 1) : item.id;
}

function describe(item: Item, status?: NodeStatus) {
  const info = actionInfo[item.action];
  const state = status ? `, ${statusWord[status.status]}` : "";
  if (!item.node) return `module ${item.id.replace(/^module\./, "")}, closed, ${info.word}, ${countsText(item.counts) || "no changes"}${state}`;
  return `${item.id}, ${info.word}, ${item.instances} instance${item.instances === 1 ? "" : "s"}${state}`;
}

function ItemNode({ data }: NodeProps<FlowNode<ItemData>>) {
  const { item, match, status } = data;
  const info = actionInfo[item.action];
  return (
    <div
      className={`gnode ${info.tone}${item.node ? "" : " closed"}${match ? " match" : ""}${status ? ` state-${status.status}` : ""}`}
      title={item.id}
    >
      <Handle type="target" position={Position.Left} />
      <span className="glyph" aria-hidden="true">
        {info.glyph}
      </span>
      <span className="addr">{item.node ? label(item) : `module.${item.id.replace(/^module\./, "")}`}</span>
      {item.node ? item.instances > 1 && <span className="gcount">×{item.instances}</span> : <span className="gcount">{countsText(item.counts)}</span>}
      {status && statusMark[status.status] && (
        <span className="gstate" aria-hidden="true">
          {statusMark[status.status]}
        </span>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

function GroupNode({ data }: NodeProps<FlowNode<{ group: PlanGroup }>>) {
  return (
    <div className="ggroup">
      <Handle type="target" position={Position.Left} />
      <span className="ggroup-name">module.{data.group.name}</span>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes = { item: ItemNode, group: GroupNode };

type GraphProps = {
  org: string;
  project: string;
  workspace: string;
  data: GetPlanGraphResponse;
  writer: boolean;
  // During or after an apply: each changing node's status, and the
  // diagnostics of failed ones.
  states?: Map<string, NodeStatus>;
  diagnostics?: Map<string, Diagnostic[]>;
  // Shown in the bar, such as replay controls.
  controls?: ReactNode;
};

export function Graph(props: GraphProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  );
}

// moduleStatus sums up the statuses inside a closed module.
function moduleStatus(states: Map<string, NodeStatus>, id: string): NodeStatus | undefined {
  const inside = [...states].filter(([node]) => node.startsWith(`${id}.`)).map(([, st]) => st);
  if (inside.length === 0) return undefined;
  const completed = inside.reduce((n, st) => n + st.completed, 0);
  const expected = inside.reduce((n, st) => n + st.expected, 0);
  const has = (s: NodeStatus["status"]) => inside.some((st) => st.status === s);
  const status = has("failed") ? "failed" : inside.every((st) => st.status === "done") ? "done" : has("running") || completed > 0 ? "running" : "waiting";
  return { status, completed, expected };
}

function Canvas({ org, project, workspace, data, writer, states, diagnostics, controls }: GraphProps) {
  const g = data.graph!;
  const closed = g.nodes.length > openLimit;
  const [open, setOpen] = useState(new Set<string>());
  const [hideUnchanged, setHideUnchanged] = useState(false);
  const [selected, setSelected] = useState<string>();
  const [query, setQuery] = useState("");
  const [hit, setHit] = useState(-1);
  const [relayout, setRelayout] = useState(0);
  // The positions, with the view they were placed for: a new view draws only
  // once its layout is ready.
  const [placed, setPlaced] = useState<{ view: View; pos: Map<string, Point> }>();
  const [saveError, setSaveError] = useState<string>();
  const stored = useRef(new Map(data.layout.map((p) => [p.id, { x: p.x, y: p.y }])));
  const replaceNext = useRef(false);
  const flow = useReactFlow();

  const view = useMemo(() => visible(g, closed, open, hideUnchanged), [g, closed, open, hideUnchanged]);

  useEffect(() => {
    let live = true;
    const replace = replaceNext.current;
    replaceNext.current = false;
    layout(view).then((elk) => {
      if (!live) return;
      const placed = place(view.items, elk, stored.current);
      for (const id of placed.added) stored.current.set(id, placed.pos.get(id)!);
      setPlaced({ view, pos: placed.pos });
      if (!writer || placed.added.length === 0) return;
      graphApi
        .saveGraphLayout({ org, project, workspace, replace, positions: placed.added.map((id) => ({ id, ...placed.pos.get(id)! })) })
        .catch(() => setSaveError("The layout was not saved. It is placed again next time."));
    });
    return () => {
      live = false;
    };
  }, [view, relayout, writer, org, project, workspace]);

  const q = query.trim().toLowerCase();
  const shown = placed?.view;
  const pos = placed?.pos;
  const matches = useMemo(() => (q && shown ? shown.items.filter((it) => it.id.toLowerCase().includes(q)).map((it) => it.id) : []), [q, shown]);

  const { nodes, edges } = useMemo(() => {
    if (!shown || !pos) return { nodes: [], edges: [] };
    const view = shown;
    const box = boxes(view, pos);
    const at = (parent: string, p: Point) => {
      const b = box.get(parent);
      return b ? { x: p.x - b.x, y: p.y - b.y } : p;
    };
    const depth = (gr: PlanGroup): number => (gr.parent ? 1 + depth(view.groups.find((x) => x.id === gr.parent)!) : 0);
    const nodes: FlowNode[] = [
      ...[...view.groups]
        .filter((gr) => box.has(gr.id))
        .sort((a, b) => depth(a) - depth(b))
        .map((gr) => {
          const b = box.get(gr.id)!;
          return {
            id: gr.id,
            type: "group",
            data: { group: gr },
            position: at(gr.parent, b),
            parentId: gr.parent || undefined,
            style: { width: b.width, height: b.height },
            selectable: false,
            focusable: false,
            ariaLabel: `module ${gr.name}`,
          };
        }),
      ...view.items.map((it) => ({
        id: it.id,
        type: "item",
        data: { item: it, match: matches.includes(it.id), status: states && (it.node ? states.get(it.id) : moduleStatus(states, it.id)) },
        position: at(it.parent, pos.get(it.id)!),
        parentId: it.parent || undefined,
        width: nodeWidth,
        height: nodeHeight,
        selected: it.id === selected,
        ariaRole: "button" as const,
        ariaLabel: describe(it, states && (it.node ? states.get(it.id) : moduleStatus(states, it.id))),
      })),
    ];
    const edges: FlowEdge[] = view.edges.map((e) => ({
      id: `${e.from}\n${e.to}`,
      source: e.from,
      target: e.to,
      className: e.explicit ? "explicit" : undefined,
      markerEnd: { type: MarkerType.ArrowClosed },
      focusable: false,
    }));
    return { nodes, edges };
  }, [pos, shown, matches, selected, states]);

  const absolute = (id: string) => pos?.get(id);
  const focus = (id: string) => {
    const p = absolute(id);
    if (p) void flow.setCenter(p.x + nodeWidth / 2, p.y + nodeHeight / 2, { zoom: Math.max(flow.getZoom(), 0.8), duration: 200 });
    requestAnimationFrame(() => document.querySelector<HTMLElement>(`.react-flow__node[data-id="${CSS.escape(id)}"]`)?.focus());
  };
  const choose = (id: string) => {
    const it = shown?.items.find((x) => x.id === id);
    if (it && !it.node) setOpen(new Set([...open, id]));
    else setSelected(id);
  };

  const keyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const id = (e.target as HTMLElement).closest(".react-flow__node")?.getAttribute("data-id");
    if (!id || !pos || !shown) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      choose(id);
      return;
    }
    const dir = { ArrowRight: [1, 0], ArrowLeft: [-1, 0], ArrowDown: [0, 1], ArrowUp: [0, -1] }[e.key];
    const from = pos.get(id);
    if (!dir || !from) return;
    e.preventDefault();
    let best: string | undefined;
    let bestScore = Infinity;
    for (const it of shown.items) {
      const p = pos.get(it.id);
      if (!p || it.id === id) continue;
      const along = (p.x - from.x) * dir[0] + (p.y - from.y) * dir[1];
      const across = Math.abs((p.x - from.x) * dir[1]) + Math.abs((p.y - from.y) * dir[0]);
      if (along <= 0) continue;
      const score = along + 2 * across;
      if (score < bestScore) [best, bestScore] = [it.id, score];
    }
    if (best) focus(best);
  };

  const search = (e: FormEvent) => {
    e.preventDefault();
    if (matches.length === 0) return;
    const next = (hit + 1) % matches.length;
    setHit(next);
    focus(matches[next]);
  };

  return (
    <>
      <div className="plan-bar">
        <span>
          <Link to="/$org/projects/$project/$workspace/runs/$number" params={{ org, project, workspace, number: String(data.number) }}>
            Plan #{data.number}
          </Link>{" "}
          · <span className="mono">{summaryText(g.summary)}</span>
        </span>
        <form className="row" role="search" onSubmit={search}>
          <label className="sr-only" htmlFor="plan-search">
            Find a resource
          </label>
          <input
            id="plan-search"
            className="mono"
            placeholder="Find a resource"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setHit(-1);
            }}
          />
          <span className="hint" aria-live="polite">
            {q ? `${matches.length} found` : ""}
          </span>
        </form>
        <label className="check">
          <input type="checkbox" checked={hideUnchanged} onChange={(e) => setHideUnchanged(e.target.checked)} /> Hide unchanged
        </label>
        {closed && open.size > 0 && (
          <button type="button" onClick={() => setOpen(new Set())}>
            Close modules
          </button>
        )}
        {controls}
        {writer && (
          <button
            type="button"
            onClick={() => {
              stored.current = new Map();
              replaceNext.current = true;
              setRelayout((n) => n + 1);
            }}
          >
            Re-layout
          </button>
        )}
      </div>
      <div className="plan-body">
        <div className="plan-canvas" onKeyDown={keyDown} aria-label="Plan graph" role="application">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            nodesDraggable={false}
            nodesConnectable={false}
            edgesFocusable={false}
            onNodeClick={(_, n) => n.type === "item" && choose(n.id)}
            onPaneClick={() => setSelected(undefined)}
            fitView
            minZoom={0.1}
            proOptions={{ hideAttribution: true }}
          >
            <Background gap={16} size={1} />
            <Controls showInteractive={false} />
          </ReactFlow>
        </div>
        <aside className="inspector" aria-label="Resource">
          {selected ? (
            <NodePanel
              org={org}
              project={project}
              workspace={workspace}
              number={data.number}
              id={selected}
              baseUrl={data.sourceBaseUrl}
              status={states?.get(selected)}
              diagnostics={diagnostics?.get(selected) ?? []}
            />
          ) : (
            <p className="muted">
              {closed ? `This plan has ${g.nodes.length} resources, so its modules start closed. ` : ""}Choose a resource to see what changes.
            </p>
          )}
        </aside>
      </div>
      <div className="plan-foot">
        {states && <span>{progressText(states)} </span>}
        {saveError ?? (closed ? "Press Enter on a module to open it. " : "")}
        Arrow keys move between resources; Enter shows one.
      </div>
    </>
  );
}

function SourceLink({ source, baseUrl }: { source?: SourceLocation; baseUrl: string }) {
  if (!source) return <span className="muted">No file: the plan has none for this resource.</span>;
  const text = `${source.file}:${source.line}`;
  if (source.remote) return <span className="mono">{text} (a module that init downloaded)</span>;
  if (!baseUrl) return <span className="mono">{text}</span>;
  return (
    <a className="mono" href={`${baseUrl}${source.file}#L${source.line}`} target="_blank" rel="noreferrer">
      {text}
    </a>
  );
}

function progressText(states: Map<string, NodeStatus>) {
  const all = [...states.values()];
  const count = (s: NodeStatus["status"]) => all.filter((st) => st.status === s).length;
  const failed = count("failed");
  return `${count("done")} of ${all.length} resources done${failed ? `, ${failed} failed` : ""}.`;
}

type PanelProps = {
  org: string;
  project: string;
  workspace: string;
  number: number;
  id: string;
  baseUrl: string;
  status?: NodeStatus;
  diagnostics: Diagnostic[];
};

function NodePanel({ org, project, workspace, number, id, baseUrl, status, diagnostics }: PanelProps) {
  const detail = useLoad(`plan-node:${org}/${project}/${workspace}/${number}/${id}`, () =>
    graphApi.getPlanNode({ org, project, workspace, number, id }),
  );
  if (detail.error) return <p className="error">{detail.error}</p>;
  const d = detail.data;
  if (!d?.node) return <p className="muted">Loading…</p>;
  const info = actionInfo[d.node.action];
  return (
    <div className="stack">
      <h2 className="mono">{d.node.id}</h2>
      <p className={info.tone}>
        <span aria-hidden="true">{info.glyph} </span>
        {info.word}
        {d.node.instances > 1 && countsText(d.node.counts) ? ` · ${countsText(d.node.counts)}` : ""}
      </p>
      {status && (
        <p className={status.status === "failed" ? "destroy" : status.status === "done" ? "create" : "muted"}>
          {statusMark[status.status] && <span aria-hidden="true">{statusMark[status.status]} </span>}
          {statusWord[status.status]}
          {status.expected > 1 ? ` · ${status.completed} of ${status.expected} steps` : ""}
        </p>
      )}
      {diagnostics.map((dg, i) => (
        <div key={i} className="diagnostic" role="alert">
          <strong>{dg.summary}</strong>
          {dg.file && <div className="mono">{`${dg.file}:${dg.line}`}</div>}
          {dg.detail && <pre className="mono">{dg.detail}</pre>}
        </div>
      ))}
      <SourceLink source={d.node.source} baseUrl={baseUrl} />
      {d.instances.length === 0 && <p className="muted">Read while planning. Nothing changes.</p>}
      {d.instances.map((inst) => (
        <section key={inst.address} className="stack" aria-label={inst.address}>
          <h3 className="mono">
            {inst.address} <span className={actionInfo[inst.action].tone}>{actionInfo[inst.action].word}</span>
          </h3>
          {inst.action === PlanAction.NO_OP && <p className="muted">No change.</p>}
          {inst.changes.length > 0 && (
            <table className="table diff">
              <thead>
                <tr>
                  <th>Attribute</th>
                  <th>Before</th>
                  <th>After</th>
                </tr>
              </thead>
              <tbody>
                {inst.changes.map((c) => (
                  <tr key={c.path}>
                    <td className="mono">
                      {c.path}
                      {c.forcesReplacement && <div className="destroy">forces replacement</div>}
                    </td>
                    <td className="mono">{c.before}</td>
                    <td className="mono">{c.after}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {inst.truncated && <p className="hint">Only the first 1000 attributes are shown.</p>}
        </section>
      ))}
    </div>
  );
}

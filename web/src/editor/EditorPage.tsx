import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent } from "react";
import { Action, ActionButton, useAction } from "../Action";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { Code as Status, ConnectError } from "@connectrpc/connect";
import { agentApi, message, projectApi } from "../api";
import { AgentSessionStatus, SaveOutcome } from "../gen/stackwright/v1/agent_pb";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useShell } from "../me";
import { Select } from "../Select";
import { useLoad } from "../useLoad";
import type { CodeHandle, OpenFile } from "./Code";

const Code = lazy(() => import("./Code"));

type Tab = OpenFile & {
  // base is the file as it was opened or last saved; a save merges from it.
  base: string;
};

type Row = { path: string; name: string; depth: number; dir: boolean };

// rows lists the tree's visible rows: folders first, then files, each
// under its folder when that folder is open.
function rows(paths: string[], open: Set<string>): Row[] {
  type Node = { dirs: Map<string, Node>; files: string[] };
  const root: Node = { dirs: new Map(), files: [] };
  for (const p of paths) {
    const parts = p.split("/");
    let n = root;
    for (const d of parts.slice(0, -1)) {
      if (!n.dirs.has(d)) n.dirs.set(d, { dirs: new Map(), files: [] });
      n = n.dirs.get(d)!;
    }
    n.files.push(parts.at(-1)!);
  }
  const out: Row[] = [];
  const walk = (n: Node, prefix: string, depth: number) => {
    for (const [name, child] of [...n.dirs].sort(([a], [b]) => a.localeCompare(b))) {
      const path = prefix + name;
      out.push({ path, name, depth, dir: true });
      if (open.has(path)) walk(child, path + "/", depth + 1);
    }
    for (const name of [...n.files].sort()) out.push({ path: prefix + name, name, depth, dir: false });
  };
  walk(root, "", 1);
  return out;
}

const parent = (path: string) => path.split("/").slice(0, -1).join("/");

export function EditorPage() {
  const { org, project, workspace } = useParams({ strict: false }) as { org: string; project: string; workspace: string };
  const { me } = useShell();
  const navigate = useNavigate();
  const writer = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.WRITER;
  const ws = useLoad(`${org}/${project}/${workspace}`, () => projectApi.getWorkspace({ org, project, workspace }));
  const sessions = useLoad(`agent-sessions:${org}/${project}/${workspace}`, () =>
    agentApi.listSessions({ org, project, workspace }).then((r) => r.sessions.filter((s) => s.status !== AgentSessionStatus.CLOSED)),
  );
  const [chosen, setChosen] = useState<string>();
  const session = chosen ?? sessions.data?.[0]?.id;
  const [paths, setPaths] = useState<string[]>();
  const [starting, setStarting] = useState(false);
  const [openDirs, setOpenDirs] = useState<Set<string>>(new Set());
  const [focused, setFocused] = useState<string>();
  const [tabs, setTabs] = useState<Tab[]>([]);
  const [active, setActive] = useState("");
  const [note, setNote] = useState<{ text: string; tone: "ok" | "error"; reload?: { path: string; text: string } }>();
  const code = useRef<CodeHandle>(null);
  const tree = useRef<HTMLUListElement>(null);
  const tablist = useRef<HTMLDivElement>(null);

  // The file list waits while the session's sandbox starts.
  useEffect(() => {
    if (!session) return;
    let stopped = false;
    let retry: ReturnType<typeof setTimeout>;
    setPaths(undefined);
    setTabs([]);
    setActive("");
    const load = async () => {
      try {
        const r = await agentApi.listFiles({ org, id: session });
        if (!stopped) {
          setPaths(r.paths);
          setStarting(false);
        }
      } catch (e) {
        if (stopped) return;
        if (e instanceof ConnectError && e.code === Status.Unavailable) {
          setStarting(true);
          retry = setTimeout(load, 1500);
        } else {
          setNote({ text: message(e), tone: "error" });
        }
      }
    };
    void load();
    return () => {
      stopped = true;
      clearTimeout(retry);
    };
  }, [org, session]);

  const visible = useMemo(() => rows(paths ?? [], openDirs), [paths, openDirs]);
  const current = tabs.find((t) => t.path === active);

  const openFile = async (path: string) => {
    if (!session) return;
    if (!tabs.some((t) => t.path === path)) {
      try {
        const { content } = await agentApi.openFile({ org, id: session, path });
        setTabs((ts) => (ts.some((t) => t.path === path) ? ts : [...ts, { path, text: content, base: content, rev: 0 }]));
      } catch (e) {
        setNote({ text: message(e), tone: "error" });
        return;
      }
    }
    setActive(path);
  };

  const save = async (): Promise<boolean> => {
    if (!session || !current) return true;
    const text = (await code.current?.format(current.path)) || current.text;
    try {
      const r = await agentApi.saveFile({ org, id: session, path: current.path, base: current.base, content: text });
      if (r.outcome === SaveOutcome.CONFLICT) {
        setNote({
          text: `${current.path} changed since you opened it, and the changes conflict. Nothing was saved.`,
          tone: "error",
          reload: { path: current.path, text: r.content },
        });
        return false;
      }
      // Text typed while the save was on its way stays; the next save
      // merges it from the new base.
      setTabs((ts) =>
        ts.map((t) =>
          t.path !== current.path ? t : t.text === current.text ? { ...t, text: r.content, base: r.content, rev: t.rev + 1 } : { ...t, base: r.content },
        ),
      );
      setNote({
        text: r.outcome === SaveOutcome.MERGED ? `Saved ${current.path}, merged with changes made since you opened it.` : `Saved ${current.path}.`,
        tone: "ok",
      });
      return true;
    } catch (e) {
      setNote({ text: message(e), tone: "error" });
      return false;
    }
  };

  const saveAndPlan = async () => {
    if (!session || !(await save())) return false;
    const { number } = await agentApi.planCheckout({ org, id: session });
    await navigate({ to: "/$org/projects/$project/$workspace/runs/$number", params: { org, project, workspace, number: String(number) } });
  };
  const saving = useAction();

  const startSession = async () => {
    try {
      const { session: s } = await agentApi.startSession({ org, project, workspace, message: "" });
      if (s) {
        sessions.reload();
        setChosen(s.id);
      }
    } catch (e) {
      setNote({ text: message(e), tone: "error" });
    }
  };

  // F6 moves focus from the file list to the tabs to the text, and round.
  const nextPane = useCallback(() => {
    const inTree = tree.current?.contains(document.activeElement);
    const inTabs = tablist.current?.contains(document.activeElement);
    if (inTree && tablist.current?.querySelector<HTMLElement>('[aria-selected="true"]')) {
      tablist.current.querySelector<HTMLElement>('[aria-selected="true"]')!.focus();
    } else if ((inTree || inTabs) && active) {
      code.current?.focus();
    } else {
      tree.current?.querySelector<HTMLElement>('[tabindex="0"]')?.focus();
    }
  }, [active]);
  // F6 works from anywhere on the page, the text included, so it listens
  // before Monaco does.
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== "F6") return;
      e.preventDefault();
      e.stopPropagation();
      nextPane();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [nextPane]);

  const onTreeKey = (e: KeyboardEvent) => {
    const i = visible.findIndex((r) => r.path === (focused ?? visible[0]?.path));
    const row = visible[i];
    if (!row) return;
    const move = (to: number) => {
      const r = visible[Math.max(0, Math.min(visible.length - 1, to))];
      setFocused(r.path);
      tree.current?.querySelector<HTMLElement>(`[data-path="${CSS.escape(r.path)}"]`)?.focus();
    };
    switch (e.key) {
      case "ArrowDown":
        move(i + 1);
        break;
      case "ArrowUp":
        move(i - 1);
        break;
      case "Home":
        move(0);
        break;
      case "End":
        move(visible.length - 1);
        break;
      case "ArrowRight":
        if (row.dir && !openDirs.has(row.path)) setOpenDirs(new Set(openDirs).add(row.path));
        else if (row.dir) move(i + 1);
        break;
      case "ArrowLeft":
        if (row.dir && openDirs.has(row.path)) {
          const next = new Set(openDirs);
          next.delete(row.path);
          setOpenDirs(next);
        } else if (parent(row.path)) {
          move(visible.findIndex((r) => r.path === parent(row.path)));
        }
        break;
      case "Enter":
      case " ":
        if (row.dir) {
          const next = new Set(openDirs);
          if (next.has(row.path)) next.delete(row.path);
          else next.add(row.path);
          setOpenDirs(next);
        } else {
          void openFile(row.path);
        }
        break;
      default:
        return;
    }
    e.preventDefault();
  };

  const onTabKey = (e: KeyboardEvent) => {
    const i = tabs.findIndex((t) => t.path === active);
    const to = e.key === "ArrowRight" ? i + 1 : e.key === "ArrowLeft" ? i - 1 : -2;
    if (to === -2 || !tabs.length) return;
    e.preventDefault();
    const t = tabs[(to + tabs.length) % tabs.length];
    setActive(t.path);
    tablist.current?.querySelector<HTMLElement>(`[data-path="${CSS.escape(t.path)}"]`)?.focus();
  };

  const closeTab = (path: string) => {
    const t = tabs.find((x) => x.path === path);
    if (t && t.text !== t.base && !window.confirm(`Close ${path} without saving?`)) return;
    const rest = tabs.filter((x) => x.path !== path);
    setTabs(rest);
    if (active === path) setActive(rest.at(-1)?.path ?? "");
  };

  if (!writer) return <p className="notice">Editing files needs the writer role.</p>;
  if (ws.data && !ws.data.project?.linkedRepo) {
    return (
      <p className="notice">
        The editor works on projects in a repository that a linked GitHub installation covers.{" "}
        <Link to="/$org/settings/github" params={{ org }}>
          Link a repository
        </Link>
        .
      </p>
    );
  }
  if (sessions.data && !session) {
    return (
      <div className="stack">
        <h1>Editor</h1>
        <p>Edit the files on a branch of their own, in a sandbox. A save changes the files there only; ask the agent to commit them.</p>
        <p>
          <button type="button" className="primary" onClick={startSession}>
            Edit files
          </button>
        </p>
        {note && <p className={`notice ${note.tone}`}>{note.text}</p>}
      </div>
    );
  }

  return (
    <div className="editor-page">
      <div className="view-bar">
        <span className="title">Editor</span>
        {sessions.data && session && (
          <Select
            aria-label="Session"
            value={session}
            onChange={setChosen}
            options={sessions.data.map((s) => ({ value: s.id, label: `${s.title || "Untitled"} · ${s.branch}` }))}
          />
        )}
        <button type="button" onClick={startSession}>
          New editing session
        </button>
        <span className="grow" />
        <ActionButton action={saving} type="button" disabled={!current} onClick={() => void saving.run(save)} title="⌘S" busy="Saving…" done="Saved">
          Save
        </ActionButton>
        <Action className="primary" disabled={!session || saving.state === "busy"} run={saveAndPlan} busy="Saving and planning…" done="Plan queued">
          Save and plan
        </Action>
      </div>
      {note && (
        <p className={`notice ${note.tone}`} role="status">
          {note.text}{" "}
          {note.reload && (
            <button
              type="button"
              onClick={() => {
                const r = note.reload!;
                setTabs((ts) => ts.map((t) => (t.path === r.path ? { ...t, text: r.text, base: r.text, rev: t.rev + 1 } : t)));
                setNote(undefined);
              }}
            >
              Load the file as it is now
            </button>
          )}
        </p>
      )}
      <div className="editor-body">
        <ul ref={tree} className="file-tree" role="tree" aria-label="Files" onKeyDown={onTreeKey}>
          {starting && <li className="muted">Starting the sandbox…</li>}
          {visible.map((r) => {
            const tabbable = r.path === (focused ?? visible[0]?.path);
            return (
              <li
                key={r.path}
                role="treeitem"
                data-path={r.path}
                aria-level={r.depth}
                aria-expanded={r.dir ? openDirs.has(r.path) : undefined}
                aria-selected={r.path === active}
                tabIndex={tabbable ? 0 : -1}
                className={r.dir ? "dir" : undefined}
                style={{ "--depth": r.depth - 1 } as CSSProperties}
                onFocus={() => setFocused(r.path)}
                onClick={() => {
                  setFocused(r.path);
                  if (!r.dir) void openFile(r.path);
                  else {
                    const next = new Set(openDirs);
                    if (next.has(r.path)) next.delete(r.path);
                    else next.add(r.path);
                    setOpenDirs(next);
                  }
                }}
              >
                {r.dir ? (openDirs.has(r.path) ? "▾ " : "▸ ") : ""}
                {r.name}
              </li>
            );
          })}
        </ul>
        <div className="editor-main">
          <div ref={tablist} className="editor-tabs" role="tablist" aria-label="Open files" onKeyDown={onTabKey}>
            {tabs.map((t) => {
              const unsaved = t.text !== t.base;
              return (
                <span key={t.path} className="editor-tab">
                  <button
                    type="button"
                    role="tab"
                    data-path={t.path}
                    aria-selected={t.path === active}
                    aria-label={unsaved ? `${t.path}, unsaved` : t.path}
                    tabIndex={t.path === active ? 0 : -1}
                    onClick={() => setActive(t.path)}
                  >
                    {t.path.split("/").at(-1)}
                    {unsaved && <span aria-hidden="true"> •</span>}
                  </button>
                  <button type="button" className="close" aria-label={`Close ${t.path}`} tabIndex={-1} onClick={() => closeTab(t.path)}>
                    ×
                  </button>
                </span>
              );
            })}
          </div>
          {session && (
            <Suspense fallback={<p className="muted">Loading the editor…</p>}>
              <Code
                ref={code}
                org={org}
                session={session}
                files={tabs}
                active={active}
                onChange={(path, text) => setTabs((ts) => ts.map((t) => (t.path === path && t.text !== text ? { ...t, text } : t)))}
                onSave={() => void saving.run(save)}
              />
            </Suspense>
          )}
          {!active && paths && <p className="muted editor-empty">Choose a file to open. F6 moves between the files, the tabs and the text.</p>}
        </div>
      </div>
    </div>
  );
}

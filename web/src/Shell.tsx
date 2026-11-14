import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link, Outlet, useNavigate, useParams, useRouter, useRouterState } from "@tanstack/react-router";
import { authApi, message, projectApi, runApi } from "./api";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";
import { Role } from "./gen/stackwright/v1/orgs_pb";
import { ShellContext } from "./me";
import { AgentPanel } from "./agent/AgentPanel";
import { Divider, paneWidth, useSavedWidth } from "./Divider";
import { Palette, type Command } from "./Palette";
import { applyTheme, savedTheme, type Theme } from "./theme";
import { useLoad } from "./useLoad";
import { Select } from "./Select";

const sidebarKey = "stackwright.sidebar";
const agentKey = "stackwright.agent";

function savedAgent() {
  try {
    return window.localStorage.getItem(agentKey) === "shown";
  } catch {
    return false;
  }
}

const sectionNames: Record<string, string> = {
  runs: "Runs",
  editor: "Editor",
  state: "State",
  variables: "Variables",
  cloud: "Cloud credentials",
  settings: "Settings",
};

function savedCollapsed() {
  try {
    return window.localStorage.getItem(sidebarKey) === "hidden";
  } catch {
    return false;
  }
}

export function Shell() {
  const navigate = useNavigate();
  const router = useRouter();
  const params = useParams({ strict: false }) as { org?: string; project?: string; workspace?: string };
  const path = useRouterState({ select: (s) => s.location.pathname });
  const [me, setMe] = useState<GetMeResponse>();
  const [error, setError] = useState<string>();
  const [palette, setPalette] = useState(false);
  const [collapsed, setCollapsed] = useState(savedCollapsed);
  const [theme, setTheme] = useState<Theme>(savedTheme);
  const [agentShown, setAgentShown] = useState(savedAgent);
  const [ask, setAsk] = useState<string>();
  const [agentWidth, setAgentWidth] = useSavedWidth("stackwright.width.agent");
  const agentPane = useRef<HTMLElement>(null);
  const showAgent = useCallback((show: (shown: boolean) => boolean) => {
    setAgentShown((shown) => {
      const next = show(shown);
      try {
        if (next) window.localStorage.setItem(agentKey, "shown");
        else window.localStorage.removeItem(agentKey);
      } catch {
        // Storage can be refused; the choice then lasts until reload.
      }
      return next;
    });
  }, []);
  const toggleAgent = useCallback(() => showAgent((shown) => !shown), [showAgent]);
  const askAgent = useCallback(
    (text: string) => {
      setAsk(text);
      showAgent(() => true);
    },
    [showAgent],
  );
  const toggleSidebar = useCallback(() => {
    setCollapsed((c) => {
      try {
        if (c) window.localStorage.removeItem(sidebarKey);
        else window.localStorage.setItem(sidebarKey, "hidden");
      } catch {
        // Storage can be refused; the choice then lasts until reload.
      }
      return !c;
    });
  }, []);

  useEffect(() => {
    authApi.getMe({}).then(setMe, async (e) => {
      if (e instanceof ConnectError && e.code === Code.Unauthenticated) {
        const { needed } = await authApi.getSetup({});
        navigate({ to: needed ? "/setup" : "/sign-in", search: needed ? {} : { next: router.state.location.href } });
        return;
      }
      setError(message(e));
    });
  }, [navigate, router]);

  const org = params.org ?? me?.orgs[0]?.slug ?? "";
  const ws = params.project && params.workspace ? { org, project: params.project, workspace: params.workspace } : undefined;
  const projects = useLoad(`projects:${org}`, () =>
    org ? projectApi.listProjects({ org }).then((r) => r.projects) : Promise.resolve([]),
  );
  const [waitingVersion, setWaitingVersion] = useState(0);
  const reloadWaiting = useCallback(() => setWaitingVersion((v) => v + 1), []);
  const waiting = useLoad(
    `waiting:${org}`,
    () => (org ? runApi.listWaiting({ org }).then((r) => r.approvals.length + r.failures.length + r.findings.length) : Promise.resolve(0)),
    waitingVersion,
  );
  useEffect(() => {
    const id = window.setInterval(reloadWaiting, 15000);
    return () => window.clearInterval(id);
  }, [reloadWaiting]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette(true);
      } else if (mod && e.key === "\\") {
        e.preventDefault();
        toggleSidebar();
      } else if (mod && e.altKey && e.code === "Digit0") {
        e.preventDefault();
        toggleAgent();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [toggleSidebar, toggleAgent]);

  const role = me?.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
  const commands = useMemo<Command[]>(() => {
    if (!me || !org) return [];
    const go = (to: string, p: Record<string, string> = {}) => () => navigate({ to, params: { org, ...p } });
    const list: Command[] = [
      { id: "projects", label: "Projects", group: "Page", run: go("/$org") },
      { id: "inbox", label: "Waiting on you", group: "Page", run: go("/$org/inbox") },
      { id: "settings", label: "Settings: General", group: "Page", run: go("/$org/settings") },
      { id: "members", label: "Settings: Members", group: "Page", run: go("/$org/settings/members") },
      { id: "tokens", label: "Settings: API tokens", group: "Page", run: go("/$org/settings/tokens") },
      { id: "variables", label: "Settings: Variables", group: "Page", run: go("/$org/settings/variables") },
      { id: "cloud", label: "Settings: Cloud accounts", group: "Page", run: go("/$org/settings/cloud") },
      { id: "servers", label: "Settings: Build servers", group: "Page", run: go("/$org/settings/build-servers") },
      { id: "github", label: "Settings: GitHub", group: "Page", run: go("/$org/settings/github") },
      { id: "agent", label: "Settings: Agent", group: "Page", run: go("/$org/settings/agent") },
      { id: "security", label: "Settings: Security", group: "Page", run: go("/$org/settings/security") },
    ];
    if (role >= Role.ADMIN) {
      list.push(
        { id: "audit", label: "Settings: Audit log", group: "Page", run: go("/$org/settings/audit") },
        { id: "new-project", label: "New project", group: "Page", run: go("/$org/projects/new") },
      );
    }
    if (me.user?.instanceAdmin) {
      list.push(
        { id: "sign-in", label: "Settings: Sign-in", group: "Page", run: go("/$org/settings/sign-in") },
        { id: "new-org", label: "New org", group: "Page", run: () => navigate({ to: "/new-org" }) },
      );
    }
    for (const p of projects.data ?? []) {
      list.push({
        id: `topology-${p.slug}`,
        label: `${p.slug} › Topology`,
        group: "Project",
        run: go("/$org/projects/$project/topology", { project: p.slug }),
      });
      for (const w of p.workspaces) {
        list.push({
          id: `ws-${p.slug}-${w.name}`,
          label: `${p.slug} › ${w.name}`,
          group: "Workspace",
          run: go("/$org/projects/$project/$workspace", { project: p.slug, workspace: w.name }),
        });
      }
    }
    for (const o of me.orgs) {
      if (o.slug !== org) {
        list.push({ id: `org-${o.slug}`, label: `Switch to ${o.name}`, group: "Org", run: () => navigate({ to: "/$org", params: { org: o.slug } }) });
      }
    }
    return list;
  }, [me, org, role, projects.data, navigate]);

  if (error) return <p className="page error">{error}</p>;
  if (!me) return null;

  const signOut = async () => {
    await authApi.signOut({});
    navigate({ to: "/sign-in" });
  };

  return (
    <ShellContext.Provider value={{ me, projects: projects.data, reloadProjects: projects.reload, waitingVersion, reloadWaiting, askAgent }}>
      <div className="frame">
        <aside id="sidebar" className="sidebar" aria-label="Sidebar" hidden={collapsed}>
          <Link to="/" className="app">
            Stackwright
          </Link>
          {org && (
            <label className="org">
              <span className="sr-only">Org</span>
              <Select
                value={org}
                onChange={(v) => (v === "+" ? navigate({ to: "/new-org" }) : navigate({ to: "/$org", params: { org: v } }))}
                options={[
                  ...me.orgs.map((o) => ({ value: o.slug, label: o.name })),
                  ...(me.user?.instanceAdmin ? [{ value: "+", label: "New org…" }] : []),
                ]}
              />
            </label>
          )}
          {org && ws && (
            <>
              <Link to="/$org" params={{ org }} className="back">
                ‹ All projects
              </Link>
              <div className="ws-name">
                <span className="muted">{ws.project}</span>
                <span className="mono">{ws.workspace}</span>
              </div>
              <nav aria-label="Workspace">
                <Link to="/$org/projects/$project/$workspace" params={ws} activeOptions={{ exact: true }}>
                  Overview
                </Link>
                <Link to="/$org/projects/$project/$workspace/runs" params={ws}>
                  Runs
                </Link>
                <Link to="/$org/projects/$project/$workspace/editor" params={ws}>
                  Editor
                </Link>
                <Link to="/$org/projects/$project/$workspace/state" params={ws}>
                  State
                </Link>
                <Link to="/$org/projects/$project/$workspace/variables" params={ws}>
                  Variables
                </Link>
                <Link to="/$org/projects/$project/$workspace/cloud" params={ws}>
                  Cloud credentials
                </Link>
                <Link to="/$org/projects/$project/$workspace/security" params={ws}>
                  Security
                </Link>
                <Link to="/$org/projects/$project/$workspace/settings" params={ws}>
                  Settings
                </Link>
              </nav>
            </>
          )}
          {org && (
            <nav aria-label="Main">
              {!ws && (
                <Link to="/$org" params={{ org }} activeOptions={{ exact: true }}>
                  Projects
                </Link>
              )}
              <Link to="/$org/inbox" params={{ org }}>
                Waiting on you
                {(waiting.data ?? 0) > 0 && (
                  <span className="count" aria-label={`${waiting.data} waiting`}>
                    {waiting.data}
                  </span>
                )}
              </Link>
              {!ws && (
                <>
                  <Link to="/$org/settings" params={{ org }}>
                    Settings
                  </Link>
                </>
              )}
            </nav>
          )}
          <div className="me">
            <label className="field">
              <span>Theme</span>
              <Select
                value={theme}
                onChange={(v) => {
                  setTheme(v as Theme);
                  applyTheme(v as Theme);
                }}
                options={[
                  { value: "system", label: "Same as the system" },
                  { value: "light", label: "Light" },
                  { value: "dark", label: "Dark" },
                ]}
              />
            </label>
            <span className="muted">{me.user?.name || me.user?.email}</span>
            <button type="button" onClick={signOut}>
              Sign out
            </button>
          </div>
        </aside>
        <div className="frame-main">
          <div className="topbar">
            <button type="button" aria-controls="sidebar" aria-expanded={!collapsed} onClick={toggleSidebar} title="⌘\">
              {collapsed ? "Show sidebar" : "Hide sidebar"}
            </button>
            {ws && <Crumbs ws={ws} rest={path.split("/").filter(Boolean).slice(4)} />}
            <span className="grow" />
            {ws && (
              <button type="button" aria-controls="agent" aria-expanded={agentShown} onClick={toggleAgent} title="⌥⌘0">
                {agentShown ? "Hide agent" : "Agent"}
              </button>
            )}
            <button type="button" className="search" onClick={() => setPalette(true)}>
              Search <kbd>⌘K</kbd>
            </button>
          </div>
          <div className="frame-body">
            <main className="page">
              <Outlet />
            </main>
            {ws && agentShown && (
              <Divider
                label="Resize the agent"
                pane={() => agentPane.current}
                edge="start"
                min={320}
                keep={360}
                onResize={setAgentWidth}
              />
            )}
            {ws && agentShown && (
              <aside id="agent" ref={agentPane} className="agent-panel" aria-label="Agent" style={paneWidth(agentWidth)}>
                <h2>Agent</h2>
                <AgentPanel org={ws.org} project={ws.project} workspace={ws.workspace} ask={ask} onAsked={() => setAsk(undefined)} />
              </aside>
            )}
          </div>
        </div>
      </div>
      {palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
    </ShellContext.Provider>
  );
}

type WorkspaceParams = { org: string; project: string; workspace: string };

// Crumbs leads from Projects to the page shown: rest is the path after the
// workspace, such as ["runs", "4"].
function Crumbs({ ws, rest }: { ws: WorkspaceParams; rest: string[] }) {
  const [section, number] = rest;
  return (
    <nav aria-label="Breadcrumbs" className="crumbs">
      <ol>
        <li>
          <Link to="/$org" params={{ org: ws.org }}>
            Projects
          </Link>
        </li>
        <li>{ws.project}</li>
        <li>
          {section ? (
            <Link to="/$org/projects/$project/$workspace" params={ws}>
              {ws.workspace}
            </Link>
          ) : (
            <span aria-current="page">{ws.workspace}</span>
          )}
        </li>
        {section === "runs" && number ? (
          <>
            <li>
              <Link to="/$org/projects/$project/$workspace/runs" params={ws}>
                Runs
              </Link>
            </li>
            <li>
              <span aria-current="page">Run #{number}</span>
            </li>
          </>
        ) : (
          section && (
            <li>
              <span aria-current="page">{sectionNames[section] ?? section}</span>
            </li>
          )
        )}
      </ol>
    </nav>
  );
}

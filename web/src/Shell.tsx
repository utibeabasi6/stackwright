import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link, Outlet, useNavigate, useParams, useRouter } from "@tanstack/react-router";
import { authApi, message, projectApi, runApi } from "./api";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";
import { Role } from "./gen/stackwright/v1/orgs_pb";
import { ShellContext } from "./me";
import { Palette, type Command } from "./Palette";
import { useTabs } from "./tabs";
import { useLoad } from "./useLoad";
import { Select } from "./Select";

export function Shell() {
  const navigate = useNavigate();
  const router = useRouter();
  const params = useParams({ strict: false }) as { org?: string; project?: string; workspace?: string };
  const [me, setMe] = useState<GetMeResponse>();
  const [error, setError] = useState<string>();
  const [palette, setPalette] = useState(false);

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
  const projects = useLoad(`projects:${org}`, () =>
    org ? projectApi.listProjects({ org }).then((r) => r.projects) : Promise.resolve([]),
  );
  const [waitingVersion, setWaitingVersion] = useState(0);
  const reloadWaiting = useCallback(() => setWaitingVersion((v) => v + 1), []);
  const waiting = useLoad(`waiting:${org}:${waitingVersion}`, () =>
    org ? runApi.listWaiting({ org }).then((r) => r.approvals.length + r.failures.length) : Promise.resolve(0),
  );
  useEffect(() => {
    const id = window.setInterval(reloadWaiting, 15000);
    return () => window.clearInterval(id);
  }, [reloadWaiting]);
  const { tabs, open, close } = useTabs(org);
  const openRef = useRef(open);
  openRef.current = open;
  const openTab = useCallback((t: { project: string; workspace: string }) => openRef.current(t), []);
  const current = useMemo(
    () => (params.project && params.workspace ? { project: params.project, workspace: params.workspace } : undefined),
    [params.project, params.workspace],
  );

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette(true);
      } else if (e.altKey && e.code === "KeyT") {
        e.preventDefault();
        setPalette(true);
      } else if (e.altKey && e.code === "KeyW" && current) {
        e.preventDefault();
        close(current);
        navigate({ to: "/$org", params: { org } });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [current, close, navigate, org]);

  const role = me?.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
  const commands = useMemo<Command[]>(() => {
    if (!me || !org) return [];
    const go = (to: string, p: Record<string, string> = {}) => () => navigate({ to, params: { org, ...p } });
    const list: Command[] = [
      { id: "projects", label: "Projects", group: "Page", run: go("/$org") },
      { id: "inbox", label: "Waiting on you", group: "Page", run: go("/$org/inbox") },
      { id: "servers", label: "Build servers", group: "Page", run: go("/$org/build-servers") },
      { id: "settings", label: "Settings: General", group: "Page", run: go("/$org/settings") },
      { id: "members", label: "Settings: Members", group: "Page", run: go("/$org/settings/members") },
      { id: "tokens", label: "Settings: API tokens", group: "Page", run: go("/$org/settings/tokens") },
      { id: "variables", label: "Settings: Variables", group: "Page", run: go("/$org/settings/variables") },
      { id: "cloud", label: "Settings: Cloud accounts", group: "Page", run: go("/$org/settings/cloud") },
      { id: "github", label: "Settings: GitHub", group: "Page", run: go("/$org/settings/github") },
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
      for (const w of p.workspaces) {
        list.push({
          id: `ws-${p.slug}-${w.name}`,
          label: `${p.slug} › ${w.name}`,
          group: "Workspace",
          run: go("/$org/projects/$project/$workspace", { project: p.slug, workspace: w.name }),
        });
      }
    }
    if (current) {
      list.push({
        id: "close-tab",
        label: "Close this tab",
        group: "Tab",
        run: () => {
          close(current);
          go("/$org")();
        },
      });
    }
    for (const o of me.orgs) {
      if (o.slug !== org) {
        list.push({ id: `org-${o.slug}`, label: `Switch to ${o.name}`, group: "Org", run: () => navigate({ to: "/$org", params: { org: o.slug } }) });
      }
    }
    return list;
  }, [me, org, role, projects.data, current, close, navigate]);

  if (error) return <p className="page error">{error}</p>;
  if (!me) return null;

  const signOut = async () => {
    await authApi.signOut({});
    navigate({ to: "/sign-in" });
  };

  return (
    <ShellContext.Provider value={{ me, projects: projects.data, reloadProjects: projects.reload, openTab, waitingVersion, reloadWaiting }}>
      <header className="shell">
        <div className="toolbar">
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
          <span className="grow" />
          {current && (
            <nav className="segmented" aria-label="Views">
              <Link
                to="/$org/projects/$project/$workspace"
                params={{ org, project: current.project, workspace: current.workspace }}
                activeOptions={{ exact: true }}
              >
                Overview
              </Link>
              <Link to="/$org/projects/$project/$workspace/plan" params={{ org, project: current.project, workspace: current.workspace }}>
                Plan
              </Link>
              <Link to="/$org/projects/$project/$workspace/runs" params={{ org, project: current.project, workspace: current.workspace }}>
                Runs
              </Link>
            </nav>
          )}
          <span className="grow" />
          {org && (
            <nav aria-label="Main" className="main-nav">
              <Link to="/$org" params={{ org }} activeOptions={{ exact: true }}>
                Projects
              </Link>
              <Link to="/$org/inbox" params={{ org }}>
                Waiting on you
                {(waiting.data ?? 0) > 0 && (
                  <span className="count" aria-label={`${waiting.data} waiting`}>
                    {waiting.data}
                  </span>
                )}
              </Link>
              <button type="button" className="search" onClick={() => setPalette(true)}>
                Search <kbd>⌘K</kbd>
              </button>
              <Link to="/$org/build-servers" params={{ org }}>
                Build servers
              </Link>
              <Link to="/$org/settings" params={{ org }}>
                Settings
              </Link>
            </nav>
          )}
          <div className="me">
            <span>{me.user?.name || me.user?.email}</span>
            <button type="button" onClick={signOut}>
              Sign out
            </button>
          </div>
        </div>
        {org && (
          <div className="tabstrip" role="tablist" aria-label="Open workspaces">
            {tabs.length === 0 && <span className="tab-empty">No open workspaces</span>}
            {tabs.map((t) => {
              const selected = current?.project === t.project && current?.workspace === t.workspace;
              return (
                <span key={`${t.project}/${t.workspace}`} className="tab" role="presentation">
                  <Link
                    role="tab"
                    aria-selected={selected}
                    to="/$org/projects/$project/$workspace"
                    params={{ org, project: t.project, workspace: t.workspace }}
                  >
                    <span className="muted">{t.project} ›</span> {t.workspace}
                  </Link>
                  <button
                    type="button"
                    className="tab-close"
                    aria-label={`Close ${t.project} › ${t.workspace}`}
                    onClick={() => {
                      close(t);
                      if (selected) navigate({ to: "/$org", params: { org } });
                    }}
                  >
                    ×
                  </button>
                </span>
              );
            })}
            <button type="button" className="tab-add" aria-label="Open a workspace" onClick={() => setPalette(true)}>
              +
            </button>
          </div>
        )}
      </header>
      <main className="page">
        <Outlet />
      </main>
      {palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
    </ShellContext.Provider>
  );
}

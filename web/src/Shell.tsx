import { useEffect, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link, Outlet, useNavigate, useParams, useRouter } from "@tanstack/react-router";
import { authApi, message } from "./api";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";
import { MeContext } from "./me";

export function Shell() {
  const navigate = useNavigate();
  const router = useRouter();
  const params = useParams({ strict: false }) as { org?: string };
  const [me, setMe] = useState<GetMeResponse>();
  const [error, setError] = useState<string>();

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

  if (error) return <p className="page error">{error}</p>;
  if (!me) return null;

  const org = params.org ?? me.orgs[0]?.slug;
  const signOut = async () => {
    await authApi.signOut({});
    navigate({ to: "/sign-in" });
  };

  return (
    <MeContext.Provider value={me}>
      <header>
        <div className="toolbar">
          <Link to="/" className="app">
            Stackwright
          </Link>
          {org && (
            <label className="org">
              <span className="sr-only">Org</span>
              <select
                value={org}
                onChange={(e) =>
                  e.target.value === "+"
                    ? navigate({ to: "/new-org" })
                    : navigate({ to: "/$org", params: { org: e.target.value } })
                }
              >
                {me.orgs.map((o) => (
                  <option key={o.slug} value={o.slug}>
                    {o.name}
                  </option>
                ))}
                {me.user?.instanceAdmin && <option value="+">New org…</option>}
              </select>
            </label>
          )}
          {org && (
            <nav aria-label="Main">
              <Link to="/$org" params={{ org }} activeOptions={{ exact: true }}>
                Projects
              </Link>
              <Link to="/$org/inbox" params={{ org }}>
                Waiting on you
              </Link>
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
        <div className="tabstrip" role="tablist" aria-label="Open workspaces">
          No open workspaces
        </div>
      </header>
      <main className="page">
        <Outlet />
      </main>
    </MeContext.Provider>
  );
}

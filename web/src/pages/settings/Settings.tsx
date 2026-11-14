import { Link, Outlet, useParams } from "@tanstack/react-router";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useMe } from "../../me";

export function Settings() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  const role = me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
  const admin = role >= Role.ADMIN;
  return (
    <>
      <h1>Settings</h1>
      <nav className="tabs" aria-label="Settings sections">
        <Link to="/$org/settings" params={{ org }} activeOptions={{ exact: true }}>
          General
        </Link>
        <Link to="/$org/settings/members" params={{ org }}>
          Members
        </Link>
        <Link to="/$org/settings/tokens" params={{ org }}>
          API tokens
        </Link>
        <Link to="/$org/settings/variables" params={{ org }}>
          Variables
        </Link>
        <Link to="/$org/settings/cloud" params={{ org }}>
          Cloud accounts
        </Link>
        <Link to="/$org/settings/build-servers" params={{ org }}>
          Build servers
        </Link>
        <Link to="/$org/settings/github" params={{ org }}>
          GitHub
        </Link>
        <Link to="/$org/settings/agent" params={{ org }}>
          Agent
        </Link>
        <Link to="/$org/settings/security" params={{ org }}>
          Security
        </Link>
        {admin && (
          <>
            <Link to="/$org/settings/audit" params={{ org }}>
              Audit log
            </Link>
            <Link to="/$org/settings/sso" params={{ org }}>
              Single sign-on
            </Link>
            <Link to="/$org/settings/notifications" params={{ org }}>
              Notifications
            </Link>
          </>
        )}
        {me.user?.instanceAdmin && (
          <Link to="/$org/settings/sign-in" params={{ org }}>
            Sign-in
          </Link>
        )}
      </nav>
      <Outlet />
    </>
  );
}

import { Navigate } from "@tanstack/react-router";
import { useMe } from "../me";

export function Home() {
  const me = useMe();
  const first = me.orgs[0];
  if (first) return <Navigate to="/$org" params={{ org: first.slug }} replace />;
  return (
    <>
      <h1>No orgs yet</h1>
      <p className="muted">You are not a member of any org. Ask an admin for an invite.</p>
    </>
  );
}

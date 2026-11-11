import { Link, Outlet } from "@tanstack/react-router";

const pages = [
  { to: "/", label: "Projects" },
  { to: "/inbox", label: "Waiting on you" },
  { to: "/build-servers", label: "Build servers" },
  { to: "/settings", label: "Settings" },
] as const;

export function Shell() {
  return (
    <>
      <header>
        <div className="toolbar">
          <Link to="/" className="app">
            Stackwright
          </Link>
          <nav aria-label="Main">
            {pages.map((p) => (
              <Link key={p.to} to={p.to} activeOptions={{ exact: true }}>
                {p.label}
              </Link>
            ))}
          </nav>
        </div>
        <div className="tabstrip" role="tablist" aria-label="Open workspaces">
          No open workspaces
        </div>
      </header>
      <main className="page">
        <Outlet />
      </main>
    </>
  );
}

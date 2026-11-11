import { createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { Shell } from "./Shell";
import { BuildServers, Inbox, Projects, Settings } from "./pages";

const root = createRootRoute({ component: Shell });

export const routeTree = root.addChildren([
  createRoute({ getParentRoute: () => root, path: "/", component: Projects }),
  createRoute({ getParentRoute: () => root, path: "/inbox", component: Inbox }),
  createRoute({ getParentRoute: () => root, path: "/build-servers", component: BuildServers }),
  createRoute({ getParentRoute: () => root, path: "/settings", component: Settings }),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

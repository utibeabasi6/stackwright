import { Outlet, createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { Shell } from "./Shell";
import { BuildServers } from "./pages/Empty";
import { Inbox } from "./pages/Inbox";
import { Run } from "./pages/Run";
import { Runs } from "./pages/Runs";
import { NewProject } from "./pages/NewProject";
import { Projects } from "./pages/Projects";
import { Workspace } from "./pages/Workspace";
import { Home } from "./pages/Home";
import { Invite } from "./pages/Invite";
import { NewOrg } from "./pages/NewOrg";
import { Setup } from "./pages/Setup";
import { SignIn } from "./pages/SignIn";
import { Audit } from "./pages/settings/Audit";
import { General } from "./pages/settings/General";
import { Members } from "./pages/settings/Members";
import { Settings } from "./pages/settings/Settings";
import { SignInProviders } from "./pages/settings/SignInProviders";
import { Tokens } from "./pages/settings/Tokens";

const root = createRootRoute({ component: Outlet });
const app = createRoute({ getParentRoute: () => root, id: "app", component: Shell });
const org = createRoute({ getParentRoute: () => app, path: "$org" });
const settings = createRoute({ getParentRoute: () => org, path: "settings", component: Settings });

export const routeTree = root.addChildren([
  createRoute({ getParentRoute: () => root, path: "/setup", component: Setup }),
  createRoute({ getParentRoute: () => root, path: "/sign-in", component: SignIn }),
  createRoute({ getParentRoute: () => root, path: "/invite/$token", component: Invite }),
  app.addChildren([
    createRoute({ getParentRoute: () => app, path: "/", component: Home }),
    createRoute({ getParentRoute: () => app, path: "/new-org", component: NewOrg }),
    org.addChildren([
      createRoute({ getParentRoute: () => org, path: "/", component: Projects }),
      createRoute({ getParentRoute: () => org, path: "inbox", component: Inbox }),
      createRoute({ getParentRoute: () => org, path: "build-servers", component: BuildServers }),
      createRoute({ getParentRoute: () => org, path: "projects/new", component: NewProject }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace", component: Workspace }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/runs", component: Runs }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/runs/$number", component: Run }),
      settings.addChildren([
        createRoute({ getParentRoute: () => settings, path: "/", component: General }),
        createRoute({ getParentRoute: () => settings, path: "members", component: Members }),
        createRoute({ getParentRoute: () => settings, path: "audit", component: Audit }),
        createRoute({ getParentRoute: () => settings, path: "tokens", component: Tokens }),
        createRoute({ getParentRoute: () => settings, path: "sign-in", component: SignInProviders }),
      ]),
    ]),
  ]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

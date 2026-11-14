import { Outlet, createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { ConfirmProvider } from "./Confirm";
import { Shell } from "./Shell";
import { EditorPage } from "./editor/EditorPage";
import { BuildServers } from "./pages/BuildServers";
import { EnrolServer } from "./pages/EnrolServer";
import { Import } from "./pages/Import";
import { ImportProgress } from "./pages/ImportProgress";
import { Inbox } from "./pages/Inbox";
import { TopologyPage } from "./topology/TopologyPage";
import { DeploymentPage, Deployments } from "./pages/Deployments";
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
import { CloudAccounts } from "./pages/settings/CloudAccounts";
import { AgentSettings } from "./pages/settings/Agent";
import { GitHub } from "./pages/settings/GitHub";
import { OrgVariables } from "./pages/settings/OrgVariables";
import { General } from "./pages/settings/General";
import { Members } from "./pages/settings/Members";
import { SecuritySettings } from "./pages/settings/Security";
import { Settings } from "./pages/settings/Settings";
import { SignInProviders } from "./pages/settings/SignInProviders";
import { Tokens } from "./pages/settings/Tokens";

const root = createRootRoute({
  component: () => (
    <ConfirmProvider>
      <Outlet />
    </ConfirmProvider>
  ),
});
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
      createRoute({ getParentRoute: () => org, path: "deployments", component: Deployments }),
      createRoute({ getParentRoute: () => org, path: "deployments/$name", component: DeploymentPage }),
      createRoute({ getParentRoute: () => org, path: "projects/new", component: NewProject }),
      createRoute({ getParentRoute: () => org, path: "import", component: Import }),
      createRoute({ getParentRoute: () => org, path: "imports/$id", component: ImportProgress }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/topology", component: TopologyPage }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace", component: () => <Workspace section="overview" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/state", component: () => <Workspace section="state" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/variables", component: () => <Workspace section="variables" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/cloud", component: () => <Workspace section="cloud" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/security", component: () => <Workspace section="security" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/settings", component: () => <Workspace section="settings" /> }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/runs", component: Runs }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/runs/$number", component: Run }),
      createRoute({ getParentRoute: () => org, path: "projects/$project/$workspace/editor", component: EditorPage }),
      settings.addChildren([
        createRoute({ getParentRoute: () => settings, path: "/", component: General }),
        createRoute({ getParentRoute: () => settings, path: "members", component: Members }),
        createRoute({ getParentRoute: () => settings, path: "audit", component: Audit }),
        createRoute({ getParentRoute: () => settings, path: "tokens", component: Tokens }),
        createRoute({ getParentRoute: () => settings, path: "variables", component: OrgVariables }),
        createRoute({ getParentRoute: () => settings, path: "cloud", component: CloudAccounts }),
        createRoute({ getParentRoute: () => settings, path: "build-servers", component: BuildServers }),
        createRoute({ getParentRoute: () => settings, path: "build-servers/enrol", component: EnrolServer }),
        createRoute({ getParentRoute: () => settings, path: "github", component: GitHub }),
        createRoute({ getParentRoute: () => settings, path: "agent", component: AgentSettings }),
        createRoute({ getParentRoute: () => settings, path: "security", component: SecuritySettings }),
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

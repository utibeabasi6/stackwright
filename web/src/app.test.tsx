import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Code, ConnectError } from "@connectrpc/connect";
import { RouterProvider, createMemoryHistory, createRouter } from "@tanstack/react-router";
import { routeTree } from "./router";
import { Role } from "./gen/stackwright/v1/orgs_pb";
import { Engine, Layout } from "./gen/stackwright/v1/projects_pb";
import { RunStatus } from "./gen/stackwright/v1/runs_pb";

const api = vi.hoisted(() => ({
  me: undefined as unknown,
  setupNeeded: false,
  invites: [] as string[],
  projects: [] as unknown[],
  locked: false,
  runs: [] as unknown[],
  events: [] as unknown[],
  approved: [] as number[],
  created: [] as unknown[],
}));

vi.mock("./api", () => ({
  message: (e: Error) => e.message,
  meta: { getVersion: async () => ({ version: "0.1.0" }) },
  authApi: {
    getMe: async () => {
      if (!api.me) throw new ConnectError("not signed in", Code.Unauthenticated);
      return api.me;
    },
    getSetup: async () => ({ needed: api.setupNeeded }),
    listSignInProviders: async () => ({ providers: [] }),
    signOut: async () => ({}),
  },
  orgApi: {
    listMembers: async () => ({
      members: [{ userId: "u1", email: "owner@a.test", name: "Owner", role: Role.OWNER }],
    }),
    listInvites: async () => ({ invites: [] }),
    createInvite: async ({ email }: { email: string }) => {
      api.invites.push(email);
      return { url: "http://127.0.0.1:8080/invite/secret-token" };
    },
  },
  instanceApi: {},
  stateApi: {
    getState: async () => ({
      latest: { id: "v3", serial: 3n, resources: 1, createdBy: "Owner" },
      lock: api.locked ? { id: "lock-1", who: "ci@runner", operation: "OperationTypeApply" } : undefined,
      address: "http://127.0.0.1:8080/state/a/network/prod",
      backendConfig: 'address        = "http://127.0.0.1:8080/state/a/network/prod"\n',
    }),
    listStateVersions: async () => ({ versions: [] }),
  },
  tokenApi: {
    listApiTokens: async () => ({ tokens: [] }),
    createApiToken: async () => ({ token: { id: "t1", name: "laptop" }, secret: "swt_secret-value" }),
  },
  runApi: {
    listRuns: async () => ({ runs: api.runs }),
    listWaiting: async () => ({ approvals: api.runs.filter((r) => (r as { status: number }).status === 3), failures: [] }),
    createRun: async (req: unknown) => {
      api.created.push(req);
      return { run: { number: 7 } };
    },
    approveRun: async ({ number }: { number: number }) => {
      api.approved.push(number);
      return { run: { ...(api.runs[0] as object), status: 4 } };
    },
    watchRun: async function* () {
      yield { run: api.runs[0], events: api.events };
    },
  },
  projectApi: {
    listProjects: async () => ({ projects: api.projects }),
    getWorkspace: async ({ project, workspace }: { project: string; workspace: string }) => {
      const p = (api.projects as { slug: string; workspaces: { name: string }[] }[]).find((x) => x.slug === project);
      const w = p?.workspaces.find((x) => x.name === workspace);
      if (!p || !w) throw new ConnectError("not found", Code.NotFound);
      return { project: p, workspace: w };
    },
  },
}));

function at(path: string) {
  const history = createMemoryHistory({ initialEntries: [path] });
  render(<RouterProvider router={createRouter({ routeTree, history })} />);
  return history;
}

function signedInAs(role: Role) {
  api.me = {
    user: { id: "u1", email: "owner@a.test", name: "Owner", instanceAdmin: true },
    orgs: [{ slug: "a", name: "Acme", role }],
  };
}

const network = {
  slug: "network",
  path: "network",
  engine: Engine.OPENTOFU,
  engineVersion: "1.10.7",
  layout: Layout.CLI_WORKSPACES,
  repoUrl: "",
  branch: "",
  sample: false,
  workspaces: [{ name: "dev" }, { name: "staging" }, { name: "prod" }],
};

beforeEach(() => {
  api.me = undefined;
  api.setupNeeded = false;
  api.invites = [];
  api.projects = [];
  api.locked = false;
  api.runs = [];
  api.events = [];
  api.approved = [];
  api.created = [];
  window.localStorage.clear();
});

test("a fresh instance sends you to setup", async () => {
  api.setupNeeded = true;
  at("/a");
  expect(await screen.findByRole("heading", { name: "Set up Stackwright" })).toBeInTheDocument();
});

test("a signed-out visitor goes to sign-in", async () => {
  at("/a/settings");
  expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
});

test("the toolbar marks the page you are on", async () => {
  signedInAs(Role.VIEWER);
  at("/a/build-servers");
  expect(await screen.findByRole("heading", { name: "Build servers" })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Build servers" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Projects" })).not.toHaveAttribute("aria-current");
});

test("an admin makes an invite link and sees it once", async () => {
  signedInAs(Role.OWNER);
  at("/a/settings/members");
  await userEvent.type(await screen.findByLabelText("Email"), "kim@a.test");
  await userEvent.click(screen.getByRole("button", { name: "Make invite link" }));
  expect(await screen.findByLabelText("Invite link")).toHaveValue("http://127.0.0.1:8080/invite/secret-token");
  expect(api.invites).toEqual(["kim@a.test"]);
});

test("a viewer sees members but no invite form", async () => {
  signedInAs(Role.VIEWER);
  at("/a/settings/members");
  expect(await screen.findByText("owner@a.test")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Make invite link" })).not.toBeInTheDocument();
});

test("a project with three CLI workspaces shows three rows", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  at("/a");
  const card = await screen.findByRole("region", { name: "Project network" });
  expect(within(card).getAllByRole("listitem")).toHaveLength(3);
});

test("an admin with no projects is offered the sample", async () => {
  signedInAs(Role.ADMIN);
  at("/a");
  expect(await screen.findByRole("button", { name: "Try the sample" })).toBeInTheDocument();
});

test("every page is in the command palette", async () => {
  signedInAs(Role.OWNER);
  api.projects = [network];
  at("/a");
  await screen.findByRole("heading", { name: "Projects" });
  await userEvent.keyboard("{Control>}k{/Control}");
  const list = await screen.findByRole("listbox");
  const labels = within(list)
    .getAllByRole("option")
    .map((o) => o.textContent);
  for (const page of [
    "Projects",
    "Waiting on you",
    "Build servers",
    "Settings: General",
    "Settings: Members",
    "Settings: Audit log",
    "Settings: Sign-in",
    "New project",
    "New org",
    "network › prod",
  ]) {
    expect(labels.some((l) => l?.startsWith(page))).toBe(true);
  }
  await userEvent.type(screen.getByRole("combobox", { name: "Search pages and workspaces" }), "staging{Enter}");
  expect(await screen.findByRole("heading", { name: "staging" })).toBeInTheDocument();
});

test("workspace tabs survive a reload and close with Alt+W", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  at("/a/projects/network/prod");
  expect(await screen.findByRole("tab", { name: /prod/ })).toHaveAttribute("aria-selected", "true");
  cleanup();

  at("/a");
  expect(await screen.findByRole("tab", { name: /prod/ })).toBeInTheDocument();
  cleanup();

  at("/a/projects/network/prod");
  await screen.findByRole("heading", { name: "prod" });
  await userEvent.keyboard("{Alt>}w{/Alt}");
  expect(await screen.findByText("No open workspaces")).toBeInTheDocument();
});

test("the workspace shows its state and the backend setup", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  api.locked = true;
  at("/a/projects/network/prod");
  expect(await screen.findByText("serial 3")).toBeInTheDocument();
  expect(screen.getByText(/Locked by ci@runner for apply/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Force unlock" })).not.toBeInTheDocument();
  expect(screen.getByLabelText("Backend config").textContent).toContain('address        = "http://127.0.0.1:8080/state/a/network/prod"');
  expect(screen.getByLabelText("Init command")).toHaveTextContent("tofu init -reconfigure -backend-config=prod.tfbackend");
});

test("an admin can force-unlock", async () => {
  signedInAs(Role.ADMIN);
  api.projects = [network];
  api.locked = true;
  at("/a/projects/network/prod");
  expect(await screen.findByRole("button", { name: "Force unlock" })).toBeInTheDocument();
});

test("a new API token is shown once", async () => {
  signedInAs(Role.VIEWER);
  at("/a/settings/tokens");
  await userEvent.type(await screen.findByLabelText("Name"), "laptop");
  await userEvent.click(screen.getByRole("button", { name: "Make token" }));
  expect(await screen.findByLabelText("New token")).toHaveTextContent("swt_secret-value");
});

const waitingRun = {
  id: "r1",
  number: 1,
  project: "network",
  workspace: "prod",
  status: RunStatus.NEEDS_APPROVAL,
  source: "network.tar.gz",
  hasChanges: true,
  adds: 3,
  changes: 1,
  destroys: 0,
  createdBy: "Owner",
};

test("a reviewer approves a run from its timeline", async () => {
  signedInAs(Role.REVIEWER);
  api.projects = [network];
  api.runs = [waitingRun];
  api.events = [
    { seq: 1n, type: "run.queued", payload: '{"source":"network.tar.gz","plan_only":false}', actor: "Owner" },
    {
      seq: 2n,
      type: "engine.planned_change",
      resourceAddr: "aws_nat_gateway.main",
      payload: '{"@message":"aws_nat_gateway.main: Plan to create","change":{"action":"create"}}',
    },
    { seq: 3n, type: "run.needs_approval", payload: "{}" },
  ];
  at("/a/projects/network/prod/runs/1");
  const timeline = await screen.findByRole("list", { name: "Timeline" });
  expect(await within(timeline).findByText("aws_nat_gateway.main: Plan to create")).toBeInTheDocument();
  expect(within(timeline).getByText("Queued by Owner, from network.tar.gz")).toBeInTheDocument();
  expect(screen.getByText("~ Needs approval")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Approve and apply" }));
  expect(api.approved).toEqual([1]);
});

test("a viewer cannot approve or cancel", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  api.runs = [waitingRun];
  at("/a/projects/network/prod/runs/1");
  expect(await screen.findByText("Waits for a reviewer to approve or reject it.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Approve and apply" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
});

test("waiting approvals show in the toolbar and on Waiting on you", async () => {
  signedInAs(Role.REVIEWER);
  api.projects = [network];
  api.runs = [waitingRun];
  at("/a/inbox");
  expect(await screen.findByText(/Run #1 waits for approval/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Review plan" })).toHaveAttribute("href", "/a/projects/network/prod/runs/1");
  expect(await screen.findByLabelText("1 waiting")).toBeInTheDocument();
});

test("a writer starts a plan from the workspace", async () => {
  signedInAs(Role.WRITER);
  api.projects = [{ ...network, sample: true }];
  at("/a/projects/network/prod");
  await userEvent.click(await screen.findByRole("checkbox", { name: /Plan only/ }));
  await userEvent.click(screen.getByRole("button", { name: "Start plan" }));
  expect(api.created).toMatchObject([{ org: "a", project: "network", workspace: "prod", planOnly: true }]);
});


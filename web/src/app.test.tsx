import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
  updates: [] as unknown[],
  agentKey: false,
  agentSteps: [] as unknown[],
  decisions: [] as unknown[],
  started: [] as string[],
  variables: [] as unknown[],
  setVariables: [] as unknown[],
}));

vi.mock("./api", () => ({
  message: (e: Error) => e.message,
  graphApi: {
    getPlanGraph: async () => {
      throw new ConnectError("no plan to show", Code.FailedPrecondition);
    },
  },
  agentApi: {
    startSession: async (req: { message: string }) => {
      api.started.push(req.message);
      return { session: { id: "s2" } };
    },
    getAgentSettings: async () => ({ settings: { hasKey: api.agentKey, monthlyCap: 50, spentThisMonth: 1.25 } }),
    listSessions: async () => ({ sessions: api.agentSteps.length ? [{ id: "s1", title: "Add a random_pet named db" }] : [] }),
    watchSession: async function* () {
      yield { session: { id: "s1", branch: "agent/add-db-s1", status: 5, spent: 0.12 }, steps: api.agentSteps };
      await new Promise(() => {});
    },
    decideProposal: async (req: unknown) => {
      api.decisions.push(req);
      return {};
    },
  },
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
  buildServerApi: {
    listBuildServers: async () => ({
      buildServers: [
        { id: "b0", name: "built-in", builtIn: true, status: 1, labels: {}, slots: 2, running: 1, version: "v0.6.0" },
        {
          id: "b1",
          name: "build-lab",
          status: 3,
          labels: { site: "lab" },
          slots: 4,
          running: 1,
          version: "v0.4.0",
          upgrade: "Builder v0.4.0 is older than the server (v0.6.0). Upgrade it.",
          sshHost: "192.168.1.40",
        },
      ],
    }),
    updateBuildServer: async (req: unknown) => {
      api.updates.push(req);
      return {};
    },
    getSSHKey: async () => ({ publicKey: "ssh-ed25519 AAAAkey stackwright-a" }),
    createJoinCommand: async () => ({ command: "echo 'swe_abcdefghijklmnop' | stackwright-builder enrol" }),
  },
  variableApi: {
    listVariables: async () => ({ variables: api.variables }),
    setVariable: async (req: unknown) => {
      api.setVariables.push(req);
      return {};
    },
  },
  githubApi: {
    getGitHub: async () => ({
      app: { name: "Stackwright test", slug: "stackwright-test", htmlUrl: "https://github.com/apps/stackwright-test", owner: "acme" },
      installations: [
        { installationId: 7n, account: "acme", linkedBy: "Owner", repos: [{ fullName: "acme/infra", defaultBranch: "main", projects: ["network"] }] },
      ],
    }),
  },
  cloudApi: {
    listCloudAccounts: async () => ({
      issuer: "",
      accounts: [
        {
          id: "c1",
          name: "prod-aws",
          aws: { planRoleArn: "arn:aws:iam::123456789012:role/plan", applyRoleArn: "arn:aws:iam::123456789012:role/apply", region: "eu-west-1" },
          workspaces: ["network/prod"],
        },
      ],
    }),
  },
  projectApi: {
    listEngineVersions: async ({ engine }: { engine: Engine }) => ({
      versions: engine === Engine.TERRAFORM ? ["1.17.2", "1.16.7", "1.5.7"] : ["1.13.9", "1.13.2"],
    }),
    createProject: async (req: { slug: string }) => {
      api.created.push(req);
      return { project: { slug: req.slug, workspaces: [{ name: "default" }] } };
    },
    listProjects: async () => ({ projects: api.projects }),
    getWorkspace: async ({ project, workspace }: { project: string; workspace: string }) => {
      const p = (api.projects as { slug: string; workspaces: { name: string }[] }[]).find((x) => x.slug === project);
      const w = p?.workspaces.find((x) => x.name === workspace);
      if (!p || !w) throw new ConnectError("not found", Code.NotFound);
      return { project: p, workspace: w };
    },
    updateWorkspace: async (req: unknown) => {
      api.updates.push(req);
      return {};
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
  api.updates = [];
  api.agentKey = false;
  api.agentSteps = [];
  api.decisions = [];
  api.started = [];
  api.variables = [];
  api.setVariables = [];
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
    "Settings: Variables",
    "Settings: Cloud accounts",
    "Settings: GitHub",
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

test("the sidebar hides and shows, and it and the theme stay chosen after a reload", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  at("/a/projects/network/prod");
  expect(await screen.findByRole("heading", { name: "prod" })).toBeInTheDocument();

  await userEvent.click(screen.getByRole("combobox", { name: "Theme" }));
  await userEvent.click(screen.getByRole("option", { name: "Dark" }));
  expect(document.documentElement.dataset.theme).toBe("dark");
  await userEvent.keyboard("{Control>}\\{/Control}");
  expect(document.getElementById("sidebar")).not.toBeVisible();
  cleanup();

  at("/a");
  await userEvent.click(await screen.findByRole("button", { name: "Show sidebar" }));
  expect(screen.getByRole("complementary", { name: "Sidebar" })).toBeVisible();
  expect(screen.getByRole("combobox", { name: "Theme" })).toHaveTextContent("Dark");
  await userEvent.click(screen.getByRole("combobox", { name: "Theme" }));
  await userEvent.click(screen.getByRole("option", { name: "Same as the system" }));
  expect(document.documentElement.dataset.theme).toBeUndefined();
});

test("the workspace shows its state and the backend setup", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  api.locked = true;
  at("/a/projects/network/prod/state");
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
  at("/a/projects/network/prod/state");
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


test("build servers show their status, labels and upgrade warning", async () => {
  signedInAs(Role.ADMIN);
  at("/a/build-servers");
  expect(await screen.findByText("Same host as Stackwright")).toBeInTheDocument();
  expect(screen.getByText("2 servers · 2 of 6 sandbox slots in use")).toBeInTheDocument();
  expect(screen.getByText("~ Draining")).toBeInTheDocument();
  expect(screen.getByText(/older than the server/)).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(1);
  await userEvent.click(screen.getByRole("button", { name: "Resume" }));
  expect(api.updates).toMatchObject([{ org: "a", id: "b1", draining: false }]);
});

test("a viewer sees build servers but cannot change them", async () => {
  signedInAs(Role.VIEWER);
  at("/a/build-servers");
  expect(await screen.findByText("build-lab")).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Enrol a server" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Drain" })).not.toBeInTheDocument();
});

test("a join command hides its token until revealed", async () => {
  signedInAs(Role.ADMIN);
  at("/a/build-servers/enrol");
  await userEvent.click(await screen.findByRole("tab", { name: "Run a command on the server" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Name" }), "lab");
  await userEvent.click(screen.getByRole("button", { name: "Make the command" }));
  const command = await screen.findByLabelText("Join command");
  expect(command.textContent).toContain("swe_abcd…");
  expect(command.textContent).not.toContain("abcdefghijklmnop");
  await userEvent.click(screen.getByRole("button", { name: "Reveal" }));
  expect(command.textContent).toContain("swe_abcdefghijklmnop");
});

test("a workspace shows its variables, and never a sensitive value", async () => {
  signedInAs(Role.ADMIN);
  api.projects = [network];
  api.variables = [
    { category: 1, key: "greeting", value: "hello-project", scope: { project: "network" } },
    { category: 2, key: "DB_PASSWORD", value: "", sensitive: true, scope: { project: "network", workspace: "prod" } },
  ];
  at("/a/projects/network/prod/variables");
  const vars = await screen.findByRole("region", { name: "Variables" });
  expect(await within(vars).findByText("hello-project")).toBeInTheDocument();
  expect(within(vars).getByText("Sensitive, hidden")).toBeInTheDocument();
  expect(within(vars).getByRole("cell", { name: "Project network" })).toBeInTheDocument();
  expect(within(vars).getByRole("cell", { name: "This workspace" })).toBeInTheDocument();
  await userEvent.type(within(vars).getByRole("textbox", { name: "Name" }), "token");
  await userEvent.type(within(vars).getByRole("textbox", { name: "Value" }), "abcdefgh");
  await userEvent.click(within(vars).getByRole("checkbox", { name: "Sensitive" }));
  await userEvent.click(within(vars).getByRole("button", { name: "Set variable" }));
  expect(api.setVariables).toMatchObject([
    { org: "a", scope: { project: "network", workspace: "prod" }, key: "token", value: "abcdefgh", sensitive: true, keepValue: false },
  ]);
});

test("an admin sets auto-apply and build server labels on the Settings page, not the Overview", async () => {
  signedInAs(Role.ADMIN);
  api.projects = [network];
  at("/a/projects/network/prod");
  await screen.findByRole("heading", { name: "Start a run" });
  expect(screen.queryByRole("checkbox", { name: /Auto-apply/ })).not.toBeInTheDocument();
  await userEvent.click(within(screen.getByRole("navigation", { name: "Workspace" })).getByRole("link", { name: "Settings" }));
  await userEvent.click(await screen.findByRole("checkbox", { name: /Auto-apply plans/ }));
  await userEvent.type(screen.getByRole("textbox", { name: "Build server labels this workspace requires" }), "region=eu");
  await userEvent.click(screen.getByRole("button", { name: "Save labels" }));
  expect(api.updates).toMatchObject([
    { org: "a", project: "network", workspace: "prod", autoApply: true },
    { org: "a", project: "network", workspace: "prod", requiredLabels: { values: { region: "eu" } } },
  ]);
});

test("the agent panel says when the org has no key", async () => {
  signedInAs(Role.WRITER);
  api.projects = [{ ...network, linkedRepo: "acme/infra" }];
  at("/a/projects/network/prod");
  await screen.findByRole("heading", { name: "prod" });
  await userEvent.keyboard("{Control>}{Alt>}0{/Alt}{/Control}");
  const panel = await screen.findByRole("complementary", { name: "Agent" });
  expect(await within(panel).findByText(/needs an OpenRouter key/)).toBeInTheDocument();
  expect(within(panel).getByRole("link", { name: "Settings › Agent" })).toHaveAttribute("href", "/a/settings/agent");
  expect(screen.getByRole("navigation", { name: "Workspace" })).toBeInTheDocument();
});

test("a proposed commit waits for a person, who commits and pushes it", async () => {
  signedInAs(Role.WRITER);
  api.projects = [{ ...network, linkedRepo: "acme/infra" }];
  api.agentKey = true;
  api.agentSteps = [
    { seq: 1n, kind: "user", payload: JSON.stringify({ text: "Add a random_pet named db" }) },
    { seq: 2n, kind: "assistant", payload: JSON.stringify({ text: "", tool_calls: [{ id: "c1", function: { name: "read_file" } }] }) },
    { seq: 3n, kind: "tool_call", payload: JSON.stringify({ id: "c1", name: "read_file", arguments: '{"path":"terraform.tfstate"}' }) },
    { seq: 4n, kind: "tool_result", payload: JSON.stringify({ id: "c1", output: "Refused: state files hold secrets", error: true }) },
    {
      seq: 5n,
      kind: "proposal",
      payload: JSON.stringify({
        message: "Add a random_pet named db",
        diff: ' main.tf | 2 ++\n 1 file changed\n\ndiff --git a/main.tf b/main.tf\n+resource "random_pet" "db" {}',
      }),
    },
  ];
  at("/a/projects/network/prod");
  await screen.findByRole("heading", { name: "prod" });
  await userEvent.click(screen.getByRole("button", { name: "Agent" }));
  const panel = await screen.findByRole("complementary", { name: "Agent" });
  expect(await within(panel).findByText("Commit these changes?")).toBeInTheDocument();
  expect(within(panel).getByText("Read terraform.tfstate").closest("li")).toHaveClass("failed");
  expect(within(panel).getByText("Waits for a commit or discard")).toBeInTheDocument();
  expect(within(panel).getByText("main.tf")).toBeInTheDocument();
  expect(within(panel).getByRole("textbox", { name: "Message" })).toBeDisabled();
  await userEvent.click(within(panel).getByRole("button", { name: "Commit and push" }));
  expect(api.decisions).toMatchObject([{ org: "a", id: "s1", proposal: 5n, approve: true }]);
});

test("with no session, Enter in the composer starts one; Shift+Enter breaks the line", async () => {
  signedInAs(Role.WRITER);
  api.projects = [{ ...network, linkedRepo: "acme/infra" }];
  api.agentKey = true;
  at("/a/projects/network/prod");
  await screen.findByRole("heading", { name: "prod" });
  await userEvent.click(screen.getByRole("button", { name: "Agent" }));
  const panel = await screen.findByRole("complementary", { name: "Agent" });
  expect(await within(panel).findByText("What should the agent change?")).toBeInTheDocument();
  const box = within(panel).getByRole("textbox", { name: "Message" });
  expect(within(panel).getByRole("button", { name: "Send" })).toBeDisabled();
  await userEvent.type(box, "Add a pet{Shift>}{Enter}{/Shift}named db{Enter}");
  expect(api.started).toEqual(["Add a pet\nnamed db"]);
});

test("cloud accounts warn when AWS cannot trust the issuer", async () => {
  signedInAs(Role.ADMIN);
  at("/a/settings/cloud");
  expect(await screen.findByText(/AWS trusts only an https issuer/)).toBeInTheDocument();
  expect(screen.getByText("network/prod")).toBeInTheDocument();
});

test("the GitHub page lists linked repositories and their projects", async () => {
  signedInAs(Role.ADMIN);
  at("/a/settings/github");
  const install = await screen.findByRole("region", { name: "Installation on acme" });
  expect(within(install).getByRole("cell", { name: "acme/infra" })).toBeInTheDocument();
  expect(within(install).getByRole("cell", { name: "network" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Install on GitHub" })).toBeInTheDocument();
});

test("a run from a pull request says so", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  api.runs = [{ ...waitingRun, trigger: "pull_request", prNumber: 12, commitSha: "2222222222222222222222222222222222222222", createdBy: "" }];
  at("/a/projects/network/prod/runs/1");
  expect(await screen.findByText("pull request #12 · 2222222")).toBeInTheDocument();
});

test("a linked project plans its branch and asks for no upload", async () => {
  signedInAs(Role.WRITER);
  api.projects = [{ ...network, repoUrl: "https://github.com/acme/infra", linkedRepo: "acme/infra", runBranch: "main" }];
  at("/a/projects/network/prod");
  const start = await screen.findByRole("region", { name: "Start a run" });
  expect(within(start).getByText(/Plans the newest commit of/)).toBeInTheDocument();
  expect(within(start).queryByLabelText(/Code/)).not.toBeInTheDocument();
  await userEvent.click(within(start).getByRole("button", { name: "Start plan" }));
  expect(api.created).toMatchObject([{ org: "a", project: "network", workspace: "prod", sourceName: "" }]);
});

test("a workspace's pages are in the sidebar, and breadcrumbs lead back", async () => {
  signedInAs(Role.ADMIN);
  api.projects = [network];
  at("/a/projects/network/prod");
  const nav = await screen.findByRole("navigation", { name: "Workspace" });
  expect(within(nav).getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "‹ All projects" })).toHaveAttribute("href", "/a");
  await userEvent.click(within(nav).getByRole("link", { name: "Variables" }));
  expect(await screen.findByRole("region", { name: "Variables" })).toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "Start a run" })).not.toBeInTheDocument();

  const crumbs = screen.getByRole("navigation", { name: "Breadcrumbs" });
  expect(within(crumbs).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Projects", "network", "prod", "Variables"]);
  await userEvent.click(within(crumbs).getByRole("link", { name: "prod" }));
  expect(await screen.findByRole("region", { name: "Start a run" })).toBeInTheDocument();
});

test("a plan error shows its file, line and detail", async () => {
  signedInAs(Role.VIEWER);
  api.projects = [network];
  api.runs = [{ ...waitingRun, status: RunStatus.FAILED }];
  api.events = [
    {
      seq: 1n,
      type: "engine.diagnostic",
      payload: JSON.stringify({
        "@message": "Error: Unsupported argument",
        diagnostic: {
          severity: "error",
          summary: "Unsupported argument",
          detail: 'An argument named "nope" is not expected here.',
          range: { filename: "main.tf", start: { line: 3 } },
        },
      }),
    },
  ];
  at("/a/projects/network/prod/runs/1");
  const timeline = await screen.findByRole("list", { name: "Timeline" });
  expect(await within(timeline).findByText("! Unsupported argument (main.tf:3)")).toBeInTheDocument();
  expect(within(timeline).getByText('An argument named "nope" is not expected here.')).toBeInTheDocument();
});

test("a new project picks the newest engine version and can start with a cloud account", async () => {
  signedInAs(Role.ADMIN);
  at("/a/projects/new");
  await userEvent.type(await screen.findByRole("textbox", { name: "Short name" }), "lab");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Version" })).toHaveTextContent("1.13.9 (newest)"));
  await userEvent.click(screen.getByRole("combobox", { name: "Engine" }));
  await userEvent.click(screen.getByRole("option", { name: "Terraform" }));
  const version = await screen.findByRole("combobox", { name: "Version" });
  await waitFor(() => expect(version).toHaveTextContent("1.17.2 (newest)"));
  await userEvent.click(await screen.findByRole("combobox", { name: /^Cloud account/ }));
  await userEvent.click(screen.getByRole("option", { name: "prod-aws (AWS eu-west-1)" }));
  await userEvent.click(screen.getByRole("button", { name: /Create/ }));
  expect(api.created).toMatchObject([{ slug: "lab", engineVersion: "1.17.2", cloudAccount: "prod-aws" }]);
});

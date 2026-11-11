import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Code, ConnectError } from "@connectrpc/connect";
import { RouterProvider, createMemoryHistory, createRouter } from "@tanstack/react-router";
import { routeTree } from "./router";
import { Role } from "./gen/stackwright/v1/orgs_pb";

const api = vi.hoisted(() => ({
  me: undefined as unknown,
  setupNeeded: false,
  invites: [] as string[],
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

beforeEach(() => {
  api.me = undefined;
  api.setupNeeded = false;
  api.invites = [];
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

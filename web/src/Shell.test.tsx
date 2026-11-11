import { render, screen } from "@testing-library/react";
import { RouterProvider, createMemoryHistory, createRouter } from "@tanstack/react-router";
import { routeTree } from "./router";

vi.mock("./api", () => ({ meta: { getVersion: async () => ({ version: "0.1.0" }) } }));

function at(path: string) {
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  render(<RouterProvider router={router} />);
}

test("the toolbar marks the page you are on", async () => {
  at("/build-servers");
  expect(await screen.findByRole("heading", { name: "Build servers" })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Build servers" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Projects" })).not.toHaveAttribute("aria-current");
});

test("settings shows the server's version", async () => {
  at("/settings");
  expect(await screen.findByText("0.1.0")).toBeInTheDocument();
});

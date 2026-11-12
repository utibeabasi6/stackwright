import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./mock";

const main = `resource "aws_nat_gateway" "main" {\n  \n}\n`;

type Saved = { path: string; base: string; content: string };

async function editor(page: Page, opts: { outcome?: string; sessions?: unknown[] } = {}) {
  const saved: Saved[] = [];
  const started: unknown[] = [];
  const session = { id: "s1", project: "lab", workspace: "default", branch: "agent/edit-s1", status: "AGENT_SESSION_STATUS_READY", title: "Edited by hand" };
  await mockApi(page, "ROLE_WRITER", {
    GetWorkspace: () => ({ project: { slug: "lab", linkedRepo: "acme/infra" }, workspace: { name: "default" } }),
    ListProjects: () => ({ projects: [{ slug: "lab", linkedRepo: "acme/infra", workspaces: [{ name: "default" }] }] }),
    ListSessions: () => ({ sessions: opts.sessions ?? [session] }),
    StartSession: (req) => {
      started.push(req);
      return { session };
    },
    ListFiles: () => ({ paths: ["main.tf", "modules/net/main.tf"] }),
    OpenFile: (req) => ({ content: req.path === "main.tf" ? main : "# net\n" }),
    SaveFile: (req) => {
      saved.push(req as Saved);
      const outcome = opts.outcome ?? "SAVE_OUTCOME_SAVED";
      return { outcome, content: outcome === "SAVE_OUTCOME_CONFLICT" ? "# the agent's version\n" : req.content };
    },
    OpenLanguageServer: () => ({ ticket: "t1" }),
  });
  // A stand-in for tofu-ls behind the server's relay.
  await page.routeWebSocket(/\/lsp\/s1$/, (ws) => {
    let first = true;
    ws.onMessage((raw) => {
      if (first) {
        first = false;
        expect(raw).toBe("t1");
        return;
      }
      const m = JSON.parse(String(raw));
      if (m.method === "initialize") ws.send(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { capabilities: {} } }));
      if (m.method === "textDocument/completion") {
        const items = ["allocation_id", "subnet_id", "connectivity_type"].map((label) => ({ label, kind: 10, detail: "optional, string" }));
        ws.send(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { isIncomplete: false, items } }));
      }
      if (m.method === "textDocument/formatting") ws.send(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: [] }));
    });
  });
  return { saved, started };
}

test("the editor works with the keyboard only, and completion offers aws_nat_gateway arguments", async ({ page }) => {
  const { saved } = await editor(page);
  await page.goto("/a/projects/lab/default/editor");
  await expect(page.getByRole("treeitem", { name: "main.tf" })).toBeVisible();
  await expect(page.getByText("Language support on")).toBeVisible();

  await page.keyboard.press("F6");
  await expect(page.getByRole("treeitem", { name: "▸ modules" })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("treeitem", { name: "main.tf" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tab", { name: "main.tf" })).toHaveAttribute("aria-selected", "true");

  await page.keyboard.press("F6");
  await expect(page.getByRole("tab", { name: "main.tf" })).toBeFocused();
  await page.keyboard.press("F6");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("End");
  await page.keyboard.press("Control+Space");
  const suggest = page.locator(".suggest-widget");
  await expect(suggest.getByText("subnet_id")).toBeVisible();
  await expect(suggest.getByText("allocation_id")).toBeVisible();
  await page.keyboard.type("subn");
  await expect(suggest.getByText("allocation_id")).toBeHidden();
  await expect(suggest.getByText("subnet_id")).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(page.locator(".view-lines")).toContainText("subnet_id");
  await expect(page.getByRole("tab", { name: "main.tf, unsaved" })).toBeVisible();

  await page.keyboard.press("ControlOrMeta+s");
  await expect(page.getByText("Saved main.tf.")).toBeVisible();
  expect(saved).toHaveLength(1);
  expect(saved[0].base).toBe(main);
  expect(saved[0].content).toContain("subnet_id");
  await expect(page.getByRole("tab", { name: "main.tf" })).toBeVisible();

  await page.keyboard.press("F6");
  await expect(page.getByRole("treeitem", { name: "main.tf" })).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("treeitem", { name: "▾ modules" })).toHaveAttribute("aria-expanded", "true");
});

test("a conflicting save writes nothing and offers the file as it is now", async ({ page }) => {
  await editor(page, { outcome: "SAVE_OUTCOME_CONFLICT" });
  await page.goto("/a/projects/lab/default/editor");
  await page.getByRole("treeitem", { name: "main.tf" }).click();
  await page.locator(".monaco-editor .view-lines").click();
  await page.keyboard.type("# mine\n");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("changed since you opened it, and the changes conflict. Nothing was saved.")).toBeVisible();
  await expect(page.getByRole("tab", { name: "main.tf, unsaved" })).toBeVisible();
  await page.getByRole("button", { name: "Load the file as it is now" }).click();
  await expect(page.getByRole("tab", { name: "main.tf" })).toBeVisible();
  await expect(page.locator(".view-lines")).toContainText("the agent's version");
});

test("with no session, Edit files starts a quiet one", async ({ page }) => {
  const { started } = await editor(page, { sessions: [] });
  await page.goto("/a/projects/lab/default/editor");
  await page.getByRole("button", { name: "Edit files" }).click();
  expect(started).toEqual([{ org: "a", project: "lab", workspace: "default" }]);
});

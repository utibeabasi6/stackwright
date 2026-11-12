import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

test("a long agent session scrolls in its panel, not the page", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const steps = [{ seq: "1", kind: "user", payload: JSON.stringify({ text: "Add an S3 trigger" }) }];
  for (let i = 0; i < 40; i++) {
    const args = JSON.stringify({ path: `f${i}.tf` });
    steps.push({ seq: String(i + 2), kind: "tool_call", payload: JSON.stringify({ id: `c${i}`, name: "read_file", arguments: args }) });
  }
  const session = { id: "s1", project: "lab", workspace: "default", branch: "agent/x", status: "AGENT_SESSION_STATUS_WORKING", title: "Add an S3 trigger" };
  await mockApi(page, "ROLE_WRITER", {
    GetWorkspace: () => ({ project: { slug: "lab", linkedRepo: "acme/infra" }, workspace: { name: "default" } }),
    ListProjects: () => ({ projects: [{ slug: "lab", linkedRepo: "acme/infra", workspaces: [{ name: "default" }] }] }),
    GetAgentSettings: () => ({ settings: { hasKey: true } }),
    ListSessions: () => ({ sessions: [session] }),
    WatchSession: () => ({ messages: [{ steps, session }] }),
  });
  await page.goto("/a/projects/lab/default");
  await page.getByRole("button", { name: "Agent" }).click();
  await expect(page.getByText("Read f39.tf")).toBeAttached();
  const { scroll, view } = await page.evaluate(() => ({ scroll: document.scrollingElement!.scrollHeight, view: innerHeight }));
  expect(scroll).toBe(view);
});

test("the agent panel resizes from its edge and keeps its width", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const session = { id: "s1", project: "lab", workspace: "default", branch: "agent/x", status: "AGENT_SESSION_STATUS_READY", title: "Add a pet" };
  await mockApi(page, "ROLE_WRITER", {
    GetWorkspace: () => ({ project: { slug: "lab", linkedRepo: "acme/infra" }, workspace: { name: "default" } }),
    ListProjects: () => ({ projects: [{ slug: "lab", linkedRepo: "acme/infra", workspaces: [{ name: "default" }] }] }),
    GetAgentSettings: () => ({ settings: { hasKey: true } }),
    ListSessions: () => ({ sessions: [session] }),
    WatchSession: () => ({ messages: [{ steps: [], session }] }),
  });
  await page.goto("/a/projects/lab/default");
  await page.getByRole("button", { name: "Agent" }).click();
  const panel = page.getByRole("complementary", { name: "Agent" });
  const width = async () => Math.round((await panel.boundingBox())!.width);
  const start = await width();
  const divider = page.getByRole("separator", { name: "Resize the agent" });
  const box = (await divider.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + 200);
  await page.mouse.down();
  await page.mouse.move(box.x - 100, box.y + 200, { steps: 5 });
  await page.mouse.up();
  expect((await width()) - start).toBeGreaterThanOrEqual(95);
  await divider.focus();
  await page.keyboard.press("End");
  const widest = await width();
  expect(1280 - widest).toBeLessThanOrEqual(360 + 220 + 2);
  await page.reload();
  await expect(panel).toBeVisible();
  expect(await width()).toBe(widest);
  await page.getByRole("separator", { name: "Resize the agent" }).press("Enter");
  expect(await width()).toBe(start);
});

import { expect, test, type Page } from "@playwright/test";
import { golden, mockApi, testdata } from "./mock";

type Event = { seq: string; type: string; resourceAddr?: string; payload: string; at: string };

const run = (status: string) => ({
  id: "r5",
  number: 5,
  project: "lab",
  workspace: "default",
  status,
  hasChanges: true,
  createdBy: "Kim",
  trigger: "ui",
});

const node = (page: Page, id: string) => page.locator(`.react-flow__node[data-id="${id}"]`);

// changing lists the nodes that an apply works on.
const changing = (name: string) => golden(name).nodes.filter((n) => n.action !== "PLAN_ACTION_NO_OP").map((n) => n.id);

test("a live apply goes on after a dropped stream, and every node ends done", async ({ page }) => {
  const events = testdata<Event[]>("actions", "apply-events.json");
  const cut = events.findIndex((e) => e.type === "engine.apply_start" && e.resourceAddr === "terraform_data.gone");
  let release = () => {};
  const gate = new Promise<void>((r) => (release = r));
  const resumedAfter: unknown[] = [];
  await mockApi(page, "ROLE_WRITER", {
    GetPlanGraph: () => ({ graph: golden("actions"), number: 5 }),
    WatchRun: async (req) => {
      if (resumedAfter.length === 0 && !req.afterSeq) {
        resumedAfter.push("first");
        return { messages: [{ run: run("RUN_STATUS_APPLYING"), events: events.slice(0, cut + 1) }], error: "the connection dropped" };
      }
      resumedAfter.push(req.afterSeq);
      await gate;
      const after = BigInt(String(req.afterSeq ?? 0));
      return { messages: [{ events: events.filter((e) => BigInt(e.seq) > after) }, { run: run("RUN_STATUS_APPLIED") }] };
    },
  });
  await page.goto("/a/projects/lab/default/runs/5");

  await expect(node(page, "terraform_data.secret")).toHaveAttribute("aria-label", /, done$/);
  await expect(node(page, "terraform_data.gone")).toHaveAttribute("aria-label", /, in progress$/);
  await expect(node(page, "data.local_file.back")).toHaveAttribute("aria-label", /, waiting$/);
  await expect(page.getByText("Applying, live")).toBeVisible();

  release();
  for (const id of changing("actions")) await expect(node(page, id)).toHaveAttribute("aria-label", /, done$/);
  expect(resumedAfter[1]).toBe(events[cut].seq);
  await expect(page.getByText(`${changing("actions").length} of ${changing("actions").length} resources done.`)).toBeVisible();
});

test("a finished apply replays, and the replay ends where the live run did", async ({ page }) => {
  await mockApi(page, "ROLE_VIEWER", {
    GetPlanGraph: () => ({ graph: golden("actions"), number: 5 }),
    WatchRun: () => ({ messages: [{ run: run("RUN_STATUS_APPLIED"), events: testdata<Event[]>("actions", "apply-events.json") }] }),
  });
  await page.goto("/a/projects/lab/default/runs/5");
  const ids = changing("actions");
  for (const id of ids) await expect(node(page, id)).toHaveAttribute("aria-label", /, done$/);
  const labels = () => Promise.all(ids.map((id) => node(page, id).getAttribute("aria-label")));
  const live = await labels();

  await page.getByRole("slider", { name: "Replay position" }).fill("0");
  for (const id of ids) await expect(node(page, id)).toHaveAttribute("aria-label", /, waiting$/);

  await page.getByRole("combobox", { name: "Replay speed" }).click();
  await page.getByRole("option", { name: "10x" }).click();
  await page.getByRole("button", { name: "Replay" }).click();
  await expect(page.getByRole("button", { name: "Pause" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Replay" })).toBeVisible({ timeout: 5000 });
  expect(await labels()).toEqual(live);
});

test("a failed node shows its diagnostic, and nodes after it stay waiting", async ({ page }) => {
  const g = golden("failing");
  await mockApi(page, "ROLE_WRITER", {
    GetPlanGraph: () => ({ graph: g, number: 5 }),
    GetPlanNode: () => ({
      node: g.nodes.find((n) => n.id === "local_file.blocked"),
      instances: [{ address: "local_file.blocked", action: "PLAN_ACTION_CREATE", changes: [] }],
    }),
    WatchRun: () => ({ messages: [{ run: run("RUN_STATUS_FAILED"), events: testdata<Event[]>("failing", "apply-events.json") }] }),
  });
  await page.goto("/a/projects/lab/default/runs/5");
  await expect(node(page, "terraform_data.first")).toHaveAttribute("aria-label", /, done$/);
  await expect(node(page, "terraform_data.after")).toHaveAttribute("aria-label", /, waiting$/);
  const blocked = node(page, "local_file.blocked");
  await expect(blocked).toHaveAttribute("aria-label", /, failed$/);
  await blocked.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("alert").filter({ hasText: "Create local file error" })).toContainText("main.tf:10");
});

test("hiding the timeline gives the graph the page, and breadcrumbs lead back to the runs", async ({ page }) => {
  await mockApi(page, "ROLE_VIEWER", {
    GetPlanGraph: () => ({ graph: golden("actions"), number: 5 }),
    WatchRun: () => ({ messages: [{ run: run("RUN_STATUS_APPLIED"), events: testdata<Event[]>("actions", "apply-events.json") }] }),
  });
  await page.goto("/a/projects/lab/default/runs/5");
  const timeline = page.getByRole("list", { name: "Timeline" });
  await expect(timeline).toBeVisible();
  const zoom = () => page.locator(".react-flow__viewport").evaluate((el: HTMLElement) => el.style.transform);
  const before = await zoom();
  await page.getByRole("button", { name: "Hide timeline" }).click();
  await expect(timeline).toBeHidden();
  await expect.poll(zoom).not.toBe(before);
  await page.keyboard.press("Escape");
  await expect(timeline).toBeVisible();

  const crumbs = page.getByRole("navigation", { name: "Breadcrumbs" });
  await expect(crumbs.getByText("Run #5")).toHaveAttribute("aria-current", "page");
  await crumbs.getByRole("link", { name: "Runs" }).click();
  await expect(page).toHaveURL(/\/a\/projects\/lab\/default\/runs$/);
});

test("the timeline resizes by drag and by keys, keeps its width, and hides with the graph", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await mockApi(page, "ROLE_VIEWER", {
    GetPlanGraph: () => ({ graph: golden("actions"), number: 5 }),
    WatchRun: () => ({ messages: [{ run: run("RUN_STATUS_APPLIED"), events: testdata<Event[]>("actions", "apply-events.json") }] }),
  });
  await page.goto("/a/projects/lab/default/runs/5");
  const timeline = page.getByRole("list", { name: "Timeline" });
  const width = async () => (await timeline.boundingBox())!.width;
  const divider = page.getByRole("separator", { name: "Resize the timeline" });
  const start = await width();

  const box = (await divider.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + 120, box.y + box.height / 2, { steps: 5 });
  await page.mouse.up();
  expect(Math.round((await width()) - start)).toBeGreaterThanOrEqual(115);

  await divider.focus();
  const dragged = await width();
  await page.keyboard.press("ArrowLeft");
  expect(Math.round(dragged - (await width()))).toBe(16);
  await page.keyboard.press("Home");
  expect(Math.round(await width())).toBe(200);

  await page.reload();
  expect(Math.round(await width())).toBe(200);
  await page.getByRole("separator", { name: "Resize the timeline" }).dblclick();
  expect(Math.round(await width())).toBe(Math.round(start));

  await page.getByRole("button", { name: "Hide graph" }).click();
  await expect(page.locator("#run-graph")).toHaveCount(0);
  await expect(page.getByRole("separator")).toHaveCount(0);
  expect(Math.round(await width())).toBeGreaterThan(Math.round(start));
  await page.getByRole("button", { name: "Hide timeline" }).click();
  await expect(timeline).toHaveCount(0);
  await expect(page.getByText("The timeline and the graph are hidden.")).toBeVisible();
  await page.getByRole("button", { name: "Show graph" }).click();
  await expect(page.locator("#run-graph")).toBeVisible();
  await expect(timeline).toHaveCount(0);
});

test("the timeline follows an apply, and stops following when scrolled up", async ({ page }) => {
  const line = (i: number): Event => ({
    seq: String(i),
    type: "engine.log",
    payload: JSON.stringify({ "@message": `line ${i}` }),
    at: new Date(Date.UTC(2026, 10, 14, 6, 0, i % 60)).toISOString(),
  });
  let release = () => {};
  const gate = new Promise<void>((r) => (release = r));
  let calls = 0;
  await mockApi(page, "ROLE_WRITER", {
    GetPlanGraph: () => ({ graph: golden("actions"), number: 5 }),
    WatchRun: async () => {
      calls++;
      if (calls === 1) {
        return { messages: [{ run: run("RUN_STATUS_APPLYING"), events: Array.from({ length: 120 }, (_, i) => line(i + 1)) }], error: "dropped" };
      }
      await gate;
      return { messages: [{ events: Array.from({ length: 120 }, (_, i) => line(i + 121)) }, { run: run("RUN_STATUS_APPLIED") }] };
    },
  });
  await page.goto("/a/projects/lab/default/runs/5");
  const timeline = page.getByRole("list", { name: "Timeline" });
  await expect(timeline.getByText("line 120", { exact: true })).toBeVisible();
  const atEnd = () => timeline.evaluate((el) => el.scrollTop + el.clientHeight >= el.scrollHeight - 2);
  expect(await atEnd()).toBe(true);

  await timeline.evaluate((el) => el.scrollTo({ top: 0 }));
  release();
  await expect(timeline.getByText("line 240", { exact: true })).toBeAttached();
  expect(await timeline.evaluate((el) => el.scrollTop)).toBeLessThan(50);
});

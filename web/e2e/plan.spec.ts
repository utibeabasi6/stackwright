import { expect, test, type Page } from "@playwright/test";
import { golden, mockApi, type Graph } from "./mock";

// big makes a plan of n resources, spread over modules of 100, with a chain
// of edges between neighbours.
function big(n: number, inModules: boolean): Graph {
  const nodes = Array.from({ length: n }, (_, i) => {
    const module = inModules ? `module.m${Math.floor(i / 100)}` : "";
    const id = `${module ? `${module}.` : ""}terraform_data.r${i}`;
    return { id, module, type: "terraform_data", name: `r${i}`, action: "PLAN_ACTION_CREATE", instances: 1, counts: [{ action: "PLAN_ACTION_CREATE", count: 1 }] };
  });
  const edges = nodes.slice(1).map((nd, i) => ({ from: nodes[i].id, to: nd.id }));
  const groups = inModules ? [...new Set(nodes.map((nd) => nd.module))].map((id) => ({ id, name: id.slice("module.".length) })) : [];
  return { nodes, edges, groups };
}

type Mock = { graph: Graph; layout?: { id: string; x: number; y: number }[]; role?: string };

async function open(page: Page, m: Mock) {
  const saves: unknown[] = [];
  let served = 0;
  await mockApi(page, m.role ?? "ROLE_WRITER", {
    GetPlanGraph: () => {
      served = Date.now();
      return { graph: m.graph, number: 3, layout: m.layout ?? [], sourceBaseUrl: "https://github.com/acme/infra/blob/abc123/" };
    },
    GetPlanNode: () => ({
      node: m.graph.nodes.find((nd) => nd.id === "terraform_data.recreate") ?? m.graph.nodes[0],
      instances: [
        {
          address: "terraform_data.recreate",
          action: "PLAN_ACTION_REPLACE",
          changes: [{ path: "triggers_replace", before: '"a"', after: '"b"', forcesReplacement: true }],
        },
      ],
    }),
    SaveGraphLayout: (req) => {
      saves.push(req);
      return {};
    },
  });
  await page.goto("/a/projects/lab/default/plan");
  return { saves, served: () => served };
}

const nodes = (page: Page) => page.locator(".react-flow__node-item");

test("every node is reached by keyboard and announced with its action", async ({ page }) => {
  const g = golden("actions");
  await open(page, { graph: g });
  await expect(nodes(page)).toHaveCount(g.nodes.length);
  await expect(page.getByRole("button", { name: "terraform_data.gone, destroy, 1 instance" })).toBeVisible();

  await page.getByPlaceholder("Find a resource").focus();
  const reached = new Set<string>();
  for (let i = 0; i < 40 && reached.size < g.nodes.length; i++) {
    await page.keyboard.press("Tab");
    const label = await page.evaluate(() => document.activeElement?.closest(".react-flow__node")?.getAttribute("aria-label"));
    if (label) reached.add(label.split(",")[0]);
  }
  expect([...reached].sort()).toEqual(g.nodes.map((n) => n.id).sort());

  const active = () => page.evaluate(() => document.activeElement?.getAttribute("data-id"));
  const from = await active();
  let moved = false;
  for (const key of ["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight"]) {
    await page.keyboard.press(key);
    moved = await expect
      .poll(active, { timeout: 500 })
      .not.toBe(from)
      .then(() => true, () => false);
    if (moved) break;
  }
  expect(moved).toBe(true);
});

test("Enter shows a resource's diff and a link to its file", async ({ page }) => {
  await open(page, { graph: golden("actions") });
  const node = page.getByRole("button", { name: /^terraform_data\.recreate, replace,/ });
  await node.focus();
  await page.keyboard.press("Enter");
  const panel = page.getByRole("complementary", { name: "Resource" });
  await expect(panel.getByText("forces replacement")).toBeVisible();
  await expect(panel.getByRole("link", { name: "main.tf:13" })).toHaveAttribute("href", "https://github.com/acme/infra/blob/abc123/main.tf#L13");
});

test("a 300-node plan draws in under 1 s", async ({ page }) => {
  const { served } = await open(page, { graph: big(300, false) });
  await page.waitForFunction(() => document.querySelectorAll(".react-flow__node-item").length === 300, undefined, { polling: "raf" });
  const ms = Date.now() - served();
  test.info().annotations.push({ type: "draw time", description: `${ms} ms` });
  expect(ms).toBeLessThan(1000);
});

test("a larger plan opens with its modules closed, and Enter opens one", async ({ page }) => {
  await open(page, { graph: big(400, true) });
  await expect(nodes(page)).toHaveCount(4);
  const m0 = page.getByRole("button", { name: /^module m0, closed, create/ });
  await m0.focus();
  await page.keyboard.press("Enter");
  await expect(nodes(page)).toHaveCount(103);
  await expect(page.getByRole("button", { name: "Close modules" })).toBeVisible();
});

test("a writer saves a new layout once, and stored positions are kept", async ({ page }) => {
  const g = golden("modules");
  const first = await open(page, { graph: g });
  await expect(nodes(page)).toHaveCount(g.nodes.length);
  await expect.poll(() => first.saves.length).toBe(1);
  const saved = (first.saves[0] as { positions: { id: string; x: number; y: number }[]; replace?: boolean }).positions;
  expect(saved.map((p) => p.id).sort()).toEqual(g.nodes.map((n) => n.id).sort());

  const moved = saved.map((p) => (p.id === "terraform_data.base" ? { ...p, y: p.y + 2000 } : p));
  const again = await open(page, { graph: g, layout: moved });
  await expect(nodes(page)).toHaveCount(g.nodes.length);
  await page.waitForTimeout(300);
  expect(again.saves).toHaveLength(0);
  const transform = await page.locator('.react-flow__node[data-id="terraform_data.base"]').evaluate((el: HTMLElement) => el.style.transform);
  const y = Number(transform.match(/,\s*([-\d.]+)px/)?.[1]);
  expect(y).toBeCloseTo(moved.find((p) => p.id === "terraform_data.base")!.y, 1);
});

test("a viewer's browser does not save the layout", async ({ page }) => {
  const g = golden("modules");
  const { saves } = await open(page, { graph: g, role: "ROLE_VIEWER" });
  await expect(nodes(page)).toHaveCount(g.nodes.length);
  await page.waitForTimeout(300);
  expect(saves).toHaveLength(0);
  await expect(page.getByRole("button", { name: "Re-layout" })).toHaveCount(0);
});

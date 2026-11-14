import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./mock";

// The Go test writes these from real plans of a two-zone VPC.
const golden = (project: string) =>
  JSON.parse(readFileSync(join(process.cwd(), "..", "internal", "topology", "testdata", `topology-${project}.json`), "utf8"));

async function open(page: Page, project: string) {
  await mockApi(page, "ROLE_VIEWER", {
    ListProjects: () => ({ projects: [{ slug: "network", workspaces: [{ name: "prod" }] }, { slug: "app", workspaces: [{ name: "prod" }] }] }),
    GetTopology: (req) => ({ topology: golden(String(req.project)), sources: [`${req.project}/prod #3`] }),
  });
  await page.goto("/a");
  await page.getByRole("region", { name: `Project ${project}` }).getByRole("link", { name: "Topology" }).click();
  return page.getByLabel("Topology", { exact: true });
}

test("a project's two-zone VPC draws its subnets, the planned NAT dashed in the create colour, and its routes", async ({ page }) => {
  const canvas = await open(page, "network");
  await expect(page).toHaveURL(/\/a\/projects\/network\/topology$/);
  for (const name of ["public-eu-west-1a", "public-eu-west-1b", "private-eu-west-1a", "private-eu-west-1b"]) {
    await expect(canvas.getByRole("button", { name: `Subnet ${name}`, exact: true })).toBeVisible();
  }
  await expect(canvas.getByText("eu-west-1a", { exact: true })).toBeVisible();

  const nat = canvas.getByRole("button", { name: "NAT gateway aws_nat_gateway.main" });
  const style = await nat.evaluate((el) => {
    const s = getComputedStyle(el);
    const probe = document.createElement("span");
    probe.style.color = "var(--create)";
    document.body.append(probe);
    const create = getComputedStyle(probe).color;
    probe.remove();
    return { border: s.borderTopStyle, color: s.borderTopColor, create };
  });
  expect(style.border).toBe("dashed");
  expect(style.color).toBe(style.create);
  await expect(canvas.locator("path.topo-edge.create")).toHaveCount(2);

  await canvas.getByRole("button", { name: "Subnet private-eu-west-1a", exact: true }).click();
  const details = page.getByRole("complementary", { name: "Details" });
  await expect(details.getByRole("row", { name: /0\.0\.0\.0\/0 aws_nat_gateway\.main/ })).toBeVisible();
  await details.getByRole("button", { name: "aws_nat_gateway.main" }).click();
  await expect(details.getByText("+ created by the plan")).toBeVisible();
  await expect(details.getByText("known after apply")).toBeVisible();
});

test("an app project shows the network it uses dimmed, and links to that project", async ({ page }) => {
  const canvas = await open(page, "app");
  const subnet = canvas.getByRole("button", { name: "Subnet public-eu-west-1a, from network" });
  await expect(subnet).toBeVisible();
  expect(Number(await subnet.evaluate((el) => getComputedStyle(el).opacity))).toBeLessThan(1);
  expect(Number(await canvas.getByRole("button", { name: "Load balancer web" }).evaluate((el) => getComputedStyle(el).opacity))).toBe(1);

  await canvas.getByRole("button", { name: "Security group web" }).click();
  const details = page.getByRole("complementary", { name: "Details" });
  await expect(details.getByRole("row", { name: /In tcp 443 0\.0\.0\.0\/0/ })).toBeVisible();
  await subnet.click({ position: { x: 12, y: 12 } });
  await details.getByRole("link", { name: "network", exact: true }).click();
  await expect(page).toHaveURL(/\/a\/projects\/network\/topology$/);
  await expect(canvas.getByRole("button", { name: "NAT gateway aws_nat_gateway.main" })).toBeVisible();
});

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

// The Go test writes this from real plans of a two-zone VPC.
const topology = JSON.parse(readFileSync(join(process.cwd(), "..", "internal", "topology", "testdata", "topology.json"), "utf8"));

test("a two-zone VPC draws its subnets, the planned NAT dashed in the create colour, and its routes", async ({ page }) => {
  await mockApi(page, "ROLE_VIEWER", {
    GetTopology: () => ({ topology, sources: ["app/prod #1", "network/prod #3"] }),
  });
  await page.goto("/a/topology");
  const canvas = page.getByLabel("Topology", { exact: true });
  for (const name of ["public-eu-west-1a", "public-eu-west-1b", "private-eu-west-1a", "private-eu-west-1b"]) {
    await expect(canvas.getByRole("button", { name: `Subnet ${name}` })).toBeVisible();
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
  await expect(canvas.locator("path.topo-edge.create")).toHaveCount(5);

  await canvas.getByRole("button", { name: "Subnet private-eu-west-1a" }).click();
  const details = page.getByRole("complementary", { name: "Details" });
  await expect(details.getByRole("row", { name: /0\.0\.0\.0\/0 aws_nat_gateway\.main/ })).toBeVisible();
  await details.getByRole("button", { name: "aws_nat_gateway.main" }).click();
  await expect(details.getByText("+ created by the plan")).toBeVisible();
  await expect(details.getByText("known after apply")).toBeVisible();

  await canvas.getByRole("button", { name: "Security group web" }).click();
  await expect(details.getByRole("row", { name: /In tcp 443 0\.0\.0\.0\/0/ })).toBeVisible();
});

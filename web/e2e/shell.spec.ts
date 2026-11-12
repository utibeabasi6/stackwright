import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

const background = (page: import("@playwright/test").Page) => page.evaluate(() => getComputedStyle(document.body).backgroundColor);

test("the chosen theme overrides the system's, and lasts after a reload", async ({ page }) => {
  await mockApi(page, "ROLE_VIEWER", {});
  await page.goto("/a");
  const system = await background(page);
  const other = (await page.evaluate(() => matchMedia("(prefers-color-scheme: dark)").matches)) ? "Light" : "Dark";

  await page.getByRole("combobox", { name: "Theme" }).click();
  const option = await page.getByRole("option", { name: other }).boundingBox();
  expect(option!.y + option!.height).toBeLessThanOrEqual(page.viewportSize()!.height);
  await page.getByRole("option", { name: other }).click();
  const chosen = await background(page);
  expect(chosen).not.toBe(system);

  await page.reload();
  await expect.poll(() => background(page)).toBe(chosen);
  await page.getByRole("combobox", { name: "Theme" }).click();
  await page.getByRole("option", { name: "Same as the system" }).click();
  expect(await background(page)).toBe(system);
});

test("the sidebar hides with a shortcut and stays hidden after a reload", async ({ page }) => {
  await mockApi(page, "ROLE_VIEWER", {});
  await page.goto("/a");
  const sidebar = page.getByRole("complementary", { name: "Sidebar" });
  await expect(sidebar).toBeVisible();
  await page.keyboard.press("ControlOrMeta+Backslash");
  await expect(sidebar).toBeHidden();
  await page.reload();
  await expect(page.getByRole("button", { name: "Show sidebar" })).toBeVisible();
  await expect(page.locator("#sidebar")).toBeHidden();
});

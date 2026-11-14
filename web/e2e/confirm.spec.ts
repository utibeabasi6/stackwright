import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

test("revoking a token asks in our own dialog; Escape keeps it, and the dialog's Revoke revokes it", async ({ page }) => {
  const revoked: unknown[] = [];
  page.on("dialog", () => {
    throw new Error("the browser's own dialog opened");
  });
  await mockApi(page, "ROLE_ADMIN", {
    ListApiTokens: () => ({ tokens: [{ id: "t1", name: "laptop" }] }),
    RevokeApiToken: (req) => {
      revoked.push(req);
      return {};
    },
  });
  await page.goto("/a/settings/tokens");
  await page.getByRole("button", { name: "Revoke" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Revoke laptop? Anything that uses it stops working.");
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  expect(revoked).toHaveLength(0);

  await page.getByRole("button", { name: "Revoke" }).click();
  await dialog.getByRole("button", { name: "Revoke" }).click();
  await expect(dialog).toBeHidden();
  await expect.poll(() => revoked.length).toBe(1);
});

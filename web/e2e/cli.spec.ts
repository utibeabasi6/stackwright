import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

test("the device page approves the CLI sign-in that shows the same code", async ({ page }) => {
  const approved: unknown[] = [];
  await mockApi(page, "ROLE_WRITER", {
    GetDevice: (req) => ((req as { userCode: string }).userCode === "BCDF-GHJK" ? { name: "laptop" } : { error: "not_found" }),
    ApproveDevice: (req) => {
      approved.push(req);
      return {};
    },
  });
  await page.goto("/device?code=BCDF-GHJK");
  const request = page.getByLabel("Sign-in request");
  await expect(request).toContainText("laptop asks to sign in as you, with code BCDF-GHJK.");
  await request.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("Approved. The CLI signs in within a few seconds")).toBeVisible();
  expect(approved).toEqual([{ userCode: "BCDF-GHJK" }]);
});

test("the CLI page has a download for each platform and signs a machine out", async ({ page }) => {
  let tokens = [{ id: "t1", name: "laptop", createdAt: "2026-11-14T08:00:00Z", lastUsedAt: "2026-11-14T09:00:00Z" }];
  const revoked: unknown[] = [];
  await mockApi(page, "ROLE_WRITER", {
    ListCliTokens: () => ({ tokens }),
    RevokeCliToken: (req) => {
      revoked.push(req);
      tokens = [];
      return {};
    },
  });
  await page.goto("/cli");
  await expect(page.getByRole("link", { name: "Download" })).toHaveCount(4);
  await expect(page.getByLabel("Install command for macOS, Apple silicon")).toContainText("/cli/stackwright-darwin-arm64 && chmod +x stackwright");
  await expect(page.getByLabel("Sign-in command")).toContainText("stackwright login -server http");
  await page.getByRole("row", { name: /laptop/ }).getByRole("button", { name: "Sign out" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Sign out" }).click();
  await expect.poll(() => revoked).toEqual([{ id: "t1" }]);
  await expect(page.getByText("No machine is signed in.")).toBeVisible();
});

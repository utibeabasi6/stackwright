import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

test("an admin sets up SAML with a group that maps to a role, and makes a SCIM token", async ({ page }) => {
  const saved: unknown[] = [];
  let sso: Record<string, unknown> | undefined;
  const response = () => ({
    sso,
    spEntityId: "https://sw.test/auth/saml/a/metadata",
    acsUrl: "https://sw.test/auth/saml/a/acs",
    spMetadataUrl: "https://sw.test/auth/saml/a/metadata",
    scimUrl: "https://sw.test/scim/v2/a",
  });
  await mockApi(page, "ROLE_ADMIN", {
    GetSso: () => response(),
    SaveSso: (req) => {
      saved.push(req);
      sso = { idpEntityId: "https://idp.test", required: true, defaultRole: "ROLE_VIEWER", groupsAttribute: "groups",
        groupRoles: [{ group: "platform-admins", role: "ROLE_ADMIN" }], certificatesExpire: ["2027-01-01T00:00:00Z"] };
      return { sso: response() };
    },
    RotateScimToken: () => ({ token: "sws_once" }),
  });
  await page.goto("/a/settings/sso");
  await expect(page.getByLabel("Assertion consumer service URL")).toHaveText("https://sw.test/auth/saml/a/acs");
  const form = page.getByRole("form", { name: "Identity provider" });
  await form.getByLabel("Metadata URL").fill("https://idp.test/metadata");
  await form.getByRole("button", { name: "Add a group" }).click();
  await form.getByLabel("Group", { exact: true }).fill("platform-admins");
  await form.getByRole("combobox", { name: "Role of the group" }).click();
  await page.getByRole("option", { name: "Admin" }).click();
  await form.getByLabel("Require SSO for every member but owners").check();
  await form.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => saved).toEqual([
    expect.objectContaining({ org: "a", metadataUrl: "https://idp.test/metadata", required: true, defaultRole: "ROLE_VIEWER",
      groupRoles: [{ group: "platform-admins", role: "ROLE_ADMIN" }] }),
  ]);
  await expect(page.getByText("Signed in through https://idp.test.")).toBeVisible();
  await page.getByRole("button", { name: "Make a token" }).click();
  await expect(page.getByLabel("SCIM token, shown once")).toHaveText("sws_once");
});

test("a member of an org that requires SSO is sent to sign in through it", async ({ page }) => {
  await mockApi(page, "ROLE_WRITER", {
    GetMe: () => ({ user: { id: "u1", email: "kim@a.test" }, orgs: [{ slug: "a", name: "Acme", role: "ROLE_WRITER", needsSso: true }] }),
  });
  await page.goto("/a/inbox");
  await expect(page.getByText("Acme signs in through its identity provider.")).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in with SSO" })).toHaveAttribute("href", "/auth/saml/a/start?next=%2Fa%2Finbox");
});

test("a webhook destination shows its signing secret once", async ({ page }) => {
  const created: unknown[] = [];
  await mockApi(page, "ROLE_ADMIN", {
    ListDestinations: () => ({ destinations: [] }),
    GetAuditExport: () => ({ retentionDays: 365 }),
    ListCloudAccounts: () => ({ accounts: [] }),
    CreateDestination: (req) => {
      created.push(req);
      return { destination: { id: "d1", name: "siem" }, secret: "whsec" };
    },
  });
  await page.goto("/a/settings/notifications");
  const form = page.getByRole("form", { name: "New destination" });
  await form.getByRole("combobox", { name: "Kind" }).click();
  await page.getByRole("option", { name: "Webhook" }).click();
  await form.getByLabel("Name").fill("siem");
  await form.getByLabel("Webhook URL").fill("https://siem.test/hook");
  await form.getByLabel("Drift found").check();
  await form.getByRole("button", { name: "Add destination" }).click();
  await expect.poll(() => created).toEqual([expect.objectContaining({ kind: "DESTINATION_KIND_WEBHOOK", name: "siem", url: "https://siem.test/hook",
    events: ["NOTIFICATION_EVENT_RUN_FAILED", "NOTIFICATION_EVENT_DRIFT_FOUND"] })]);
  await expect(page.getByLabel("The webhook's signing secret, shown once")).toHaveText("whsec");
});

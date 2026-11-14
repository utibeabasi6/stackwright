import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

const ws = (project: string) => ({ project, workspace: "prod" });

test("a deployment run shows its steps in order and applies with one approval", async ({ page }) => {
  const decided: unknown[] = [];
  const run = (status: string) => ({
    number: 1,
    status,
    createdBy: "Kim",
    steps: [
      { workspace: ws("kms"), runNumber: 3, runStatus: "RUN_STATUS_NEEDS_APPROVAL", hasChanges: true },
      { workspace: ws("secrets"), waitingFor: [ws("kms")] },
      { workspace: ws("eks"), waitingFor: [ws("secrets")] },
    ],
  });
  let status = "DEPLOYMENT_RUN_STATUS_NEEDS_APPROVAL";
  await mockApi(page, "ROLE_REVIEWER", {
    ListProjects: () => ({ projects: ["kms", "secrets", "eks"].map((slug) => ({ slug, workspaces: [{ name: "prod" }] })) }),
    GetDeployment: () => ({
      deployment: {
        name: "platform",
        steps: [ws("kms"), ws("secrets"), ws("eks")],
        links: [
          { from: ws("kms"), to: ws("secrets"), output: "key_arn", variable: "kms_key_arn" },
          { from: ws("secrets"), to: ws("eks"), output: "secret_arn", variable: "secret_arn" },
        ],
      },
      runs: [run(status)],
    }),
    ApproveDeploymentRun: (req) => {
      decided.push(req);
      status = "DEPLOYMENT_RUN_STATUS_APPLYING";
      return { run: run(status) };
    },
  });
  await page.goto("/a/deployments/platform");
  const card = page.getByLabel("Deployment run 1");
  await expect(card.locator(".pill")).toHaveText("~ Needs approval");
  await expect(card.getByRole("listitem")).toHaveText([/kms\/prod #3/, /secrets\/prod waits for kms\/prod/, /eks\/prod waits for secrets\/prod/]);
  await page.getByRole("button", { name: "Approve and apply every step" }).click();
  await expect.poll(() => decided).toEqual([{ org: "a", name: "platform", number: 1 }]);
  await expect(card.locator(".pill")).toHaveText("Applying");
});

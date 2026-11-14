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

test("a deployment that a Stack's files define shows its steps and cannot be edited", async ({ page }) => {
  const sw = (workspace: string) => ({ project: "shop", workspace });
  await mockApi(page, "ROLE_OWNER", {
    ListProjects: () => ({ projects: [{ slug: "shop", layout: "LAYOUT_STACKS", workspaces: [{ name: "dev-network" }, { name: "dev-app-api" }] }] }),
    GetDeployment: () => ({
      deployment: {
        name: "shop-dev",
        steps: [sw("dev-network"), sw("dev-app-api")],
        links: [{ from: sw("dev-network"), to: sw("dev-app-api") }],
        stackProject: "shop",
        stackDeployment: "dev",
        stackGroup: "canary",
      },
      runs: [],
    }),
  });
  await page.goto("/a/deployments/shop-dev");
  const steps = page.getByLabel("Steps");
  await expect(steps).toContainText("The Terraform Stack in project shop defines this deployment as dev, in group canary.");
  await expect(steps.getByRole("listitem")).toHaveText(["dev-network", "dev-app-api"]);
  await expect(page.getByRole("button", { name: "Save deployment" })).toHaveCount(0);
  await expect(page.getByLabel("Workspace to add")).toHaveCount(0);
});

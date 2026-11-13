import { expect, test } from "@playwright/test";
import { mockApi } from "./mock";

const project = (slug: string, labels: Record<string, string>) => ({ slug, labels, workspaces: [{ name: "default" }] });

test("the import wizard scans, imports and shows each first plan, and a reload keeps it", async ({ page }) => {
  const started: unknown[] = [];
  let polls = 0;
  const imp = (status: string) => ({
    import: {
      id: "i1",
      repo: "acme/infra",
      commit: "1111111abc",
      createdBy: "Kim",
      workspaces: [
        { project: "envs-dev", workspace: "default", created: true, runNumber: 1, runStatus: status, hasChanges: false },
        { project: "envs-prod", workspace: "default", created: true, runNumber: 1, runStatus: status, hasChanges: false },
      ],
    },
  });
  await mockApi(page, "ROLE_ADMIN", {
    GetGitHub: () => ({ installations: [{ installationId: "7", account: "acme", repos: [{ fullName: "acme/infra" }] }] }),
    ListEngineVersions: () => ({ versions: ["1.10.7", "1.9.1"] }),
    ScanRepository: () => ({
      commit: "1111111abc",
      roots: [
        { path: "envs/dev", workspaces: ["default"], stateBackend: "STATE_BACKEND_CODE", backend: "s3", engine: "ENGINE_OPENTOFU", slug: "envs-dev",
          labels: { repo: "acme/infra", path: "envs/dev", env: "dev" } },
        { path: "envs/prod", workspaces: ["default"], stateBackend: "STATE_BACKEND_CODE", backend: "s3", engine: "ENGINE_OPENTOFU", slug: "envs-prod",
          labels: { repo: "acme/infra", path: "envs/prod", env: "prod" } },
        { path: "legacy", workspaces: ["default"], stateBackend: "STATE_BACKEND_GIT", backend: "local", engine: "ENGINE_OPENTOFU", slug: "legacy",
          labels: {}, committedState: true, existingProject: "legacy" },
      ],
      modules: ["modules/network"],
      terragruntUnits: ["live/prod/vpc"],
    }),
    StartImport: (req) => {
      started.push(req);
      return imp("RUN_STATUS_PLANNING");
    },
    GetImport: () => (++polls < 2 ? imp("RUN_STATUS_PLANNING") : imp("RUN_STATUS_PLANNED")),
  });
  await page.goto("/a/import");
  await page.getByRole("combobox", { name: "Repository" }).click();
  await page.getByRole("option", { name: "acme/infra" }).click();
  await page.getByRole("button", { name: "Scan" }).click();

  await expect(page.getByText("Runs use its s3 backend, as the code says.").first()).toBeVisible();
  await expect(page.getByText("Its state is committed to Git: runs plan from it and never apply.")).toBeVisible();
  await expect(page.getByText("Imported as legacy")).toBeVisible();
  await expect(page.getByText("live/prod/vpc")).toBeVisible();
  const legacy = page.getByRole("region", { name: "Folder legacy" }).getByRole("checkbox");
  await expect(legacy).not.toBeChecked();

  await page.getByRole("region", { name: "Folder envs/dev" }).getByRole("textbox", { name: "Labels" }).fill("env=dev, team=platform");
  await page.getByRole("button", { name: "Import 2 folders" }).click();
  await expect(page).toHaveURL(/\/a\/imports\/i1$/);
  expect(started).toMatchObject([
    {
      repo: "acme/infra",
      commit: "1111111abc",
      roots: [
        { path: "envs/dev", slug: "envs-dev", engine: "ENGINE_OPENTOFU", engineVersion: "1.10.7", labels: { env: "dev", team: "platform" } },
        { path: "envs/prod", slug: "envs-prod", engineVersion: "1.10.7" },
      ],
    },
  ]);
  await expect(page.getByText("2 of 2 first plans show no changes.")).toBeVisible();
  await expect(page.getByRole("link", { name: "#1 ✓ no changes" })).toHaveCount(2);

  await page.reload();
  await expect(page.getByText("2 of 2 first plans show no changes.")).toBeVisible();
});

test("projects group by label keys, nest in the order chosen, and keep the choice", async ({ page }) => {
  await mockApi(page, "ROLE_ADMIN", {
    ListProjects: () => ({
      projects: [
        project("api-dev", { env: "dev", app: "api", path: "envs/dev/api" }),
        project("api-prod", { env: "prod", app: "api", path: "envs/prod/api" }),
        project("web-prod", { env: "prod", app: "web", path: "envs/prod/web" }),
        project("tools", {}),
      ],
    }),
  });
  await page.goto("/a");
  const by = page.getByRole("group", { name: "Group by" });
  await by.getByRole("button", { name: "env" }).click();
  await by.getByRole("button", { name: "app" }).click();
  const prod = page.getByRole("region", { name: "env=prod" });
  await expect(prod.getByRole("region", { name: "app=web" }).getByRole("region", { name: "Project web-prod" })).toBeVisible();
  await expect(page.getByRole("region", { name: "No env" }).getByRole("region", { name: "Project tools" })).toBeVisible();

  await page.reload();
  await expect(page.getByRole("region", { name: "env=dev" }).getByRole("region", { name: "app=api" })).toBeVisible();
  await by.getByRole("button", { name: "env" }).click();
  await by.getByRole("button", { name: "app" }).click();
  await by.getByRole("button", { name: "path" }).click();
  await expect(page.getByRole("region", { name: "envs" }).getByRole("region", { name: "prod" }).getByRole("region", { name: "Project web-prod" })).toBeVisible();
});

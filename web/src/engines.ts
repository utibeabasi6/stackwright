import { Engine, Layout, type Project } from "./gen/stackwright/v1/projects_pb";

export const engineName = (e: Engine) => (e === Engine.TERRAFORM ? "Terraform" : "OpenTofu");
const layouts: Partial<Record<Layout, string>> = {
  [Layout.PLAIN]: "Plain folder",
  [Layout.CLI_WORKSPACES]: "CLI workspaces",
  [Layout.TERRAGRUNT]: "Terragrunt",
  [Layout.STACKS]: "Terraform Stacks",
};
export const layoutName = (l: Layout) => layouts[l] ?? "Plain folder";
export const describe = (p: Project) => `${engineName(p.engine)} ${p.engineVersion} · ${layoutName(p.layout)}`;

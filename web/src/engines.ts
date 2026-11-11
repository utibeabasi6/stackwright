import { Engine, Layout, type Project } from "./gen/stackwright/v1/projects_pb";

export const engineName = (e: Engine) => (e === Engine.TERRAFORM ? "Terraform" : "OpenTofu");
export const layoutName = (l: Layout) => (l === Layout.PLAIN ? "Plain folder" : "CLI workspaces");
export const describe = (p: Project) => `${engineName(p.engine)} ${p.engineVersion} · ${layoutName(p.layout)}`;

-- +goose Up

-- Terraform Stacks: a project whose files sync into a workspace for each
-- component instance and a deployment for each Stack deployment (adr/0043).

ALTER TABLE projects DROP CONSTRAINT projects_layout_check;
ALTER TABLE projects ADD CONSTRAINT projects_layout_check CHECK (layout IN ('plain', 'cli_workspaces', 'terragrunt', 'stacks'));
-- Why the last sync of a Stack's files failed, until one passes.
ALTER TABLE projects ADD COLUMN stack_error text;

CREATE TABLE stack_instances (
  workspace_id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  project_id uuid NOT NULL,
  deployment text NOT NULL,
  -- '' for the workspace of the deployment's stack outputs.
  component text NOT NULL,
  instance_key text NOT NULL DEFAULT '',
  keyed boolean NOT NULL DEFAULT false,
  removed boolean NOT NULL DEFAULT false,
  -- The components it reads, each with whether it has for_each.
  needs jsonb NOT NULL DEFAULT '{}',
  -- The identity_token blocks, each with its audience.
  identity_tokens jsonb NOT NULL DEFAULT '{}',
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
  UNIQUE (project_id, deployment, component, instance_key)
);
ALTER TABLE stack_instances ENABLE ROW LEVEL SECURITY;
CREATE POLICY stack_instances_org ON stack_instances USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON stack_instances TO stackwright_app;

-- A deployment that a Stack's files define; the web app does not change it.
ALTER TABLE deployments ADD COLUMN project_id uuid;
ALTER TABLE deployments ADD COLUMN stack_deployment text;
ALTER TABLE deployments ADD COLUMN stack_group text;
ALTER TABLE deployments ADD CONSTRAINT deployments_project_fkey FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE;
ALTER TABLE deployments ADD CONSTRAINT deployments_stack_check CHECK ((project_id IS NULL) = (stack_deployment IS NULL));
CREATE UNIQUE INDEX deployments_stack ON deployments (project_id, stack_deployment) WHERE archived_at IS NULL;

-- The code that every step of a deployment run plans, when the run starts
-- from one commit, as a push does: every component of a Stack plans the
-- files that the sync read.
ALTER TABLE deployment_runs ADD COLUMN code jsonb;

-- A step of a removed block's instance plans with -destroy.
ALTER TABLE deployment_steps ADD COLUMN destroy boolean NOT NULL DEFAULT false;

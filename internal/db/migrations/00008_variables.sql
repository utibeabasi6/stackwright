-- +goose Up

-- An org variable has no project and no workspace. A project variable has a
-- project. A workspace variable has a workspace. A sensitive value is sealed
-- with the org's data key, and its plain column is NULL.
CREATE TABLE variables (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  project_id uuid,
  workspace_id uuid,
  category text NOT NULL CHECK (category IN ('terraform', 'env')),
  key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_-]{0,127}$'),
  sensitive boolean NOT NULL,
  value text,
  value_sealed bytea,
  description text NOT NULL DEFAULT '' CHECK (length(description) <= 200),
  updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  CHECK (project_id IS NULL OR workspace_id IS NULL),
  CHECK (sensitive = (value IS NULL) AND sensitive = (value_sealed IS NOT NULL))
);
CREATE UNIQUE INDEX variables_scope_key ON variables (org_id, project_id, workspace_id, category, key) NULLS NOT DISTINCT;

ALTER TABLE variables ENABLE ROW LEVEL SECURITY;
ALTER TABLE variables FORCE ROW LEVEL SECURITY;
CREATE POLICY variables_org ON variables USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON variables TO stackwright_app;

CREATE TABLE cloud_accounts (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  provider text NOT NULL CHECK (provider = 'aws'),
  aws_plan_role_arn text NOT NULL,
  aws_apply_role_arn text NOT NULL,
  aws_region text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name),
  UNIQUE (id, org_id),
  CHECK (aws_plan_role_arn <> aws_apply_role_arn)
);

ALTER TABLE cloud_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY cloud_accounts_org ON cloud_accounts USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, DELETE ON cloud_accounts TO stackwright_app;

ALTER TABLE workspaces ADD COLUMN cloud_account_id uuid;
ALTER TABLE workspaces ADD FOREIGN KEY (cloud_account_id, org_id) REFERENCES cloud_accounts (id, org_id);

-- Keys that sign each job's web identity token. The JWKS lists every row;
-- the newest signs. private_key is sealed with the instance data key.
CREATE TABLE oidc_keys (
  kid text PRIMARY KEY,
  private_key bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON oidc_keys TO stackwright_app;

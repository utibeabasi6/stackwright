-- +goose Up

CREATE TABLE projects (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  slug text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  path text NOT NULL DEFAULT '' CHECK (length(path) <= 200),
  engine text NOT NULL CHECK (engine IN ('opentofu', 'terraform')),
  engine_version text NOT NULL CHECK (engine_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
  layout text NOT NULL CHECK (layout IN ('plain', 'cli_workspaces')),
  repo_url text NOT NULL DEFAULT '' CHECK (length(repo_url) <= 300),
  branch text NOT NULL DEFAULT '' CHECK (length(branch) <= 200),
  sample boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz,
  UNIQUE (id, org_id)
);
CREATE UNIQUE INDEX projects_live_slug_key ON projects (org_id, slug) WHERE archived_at IS NULL;

CREATE TABLE workspaces (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  project_id uuid NOT NULL,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9_-]{0,38}[a-z0-9])?$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz,
  FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX workspaces_live_name_key ON workspaces (project_id, name) WHERE archived_at IS NULL;
CREATE INDEX workspaces_org_idx ON workspaces (org_id);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY projects_org ON projects USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;
CREATE POLICY workspaces_org ON workspaces USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

GRANT SELECT, INSERT, UPDATE ON projects, workspaces TO stackwright_app;

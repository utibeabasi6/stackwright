-- +goose Up

CREATE TABLE api_tokens (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
  token_hash bytea NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  last_used_at timestamptz,
  revoked_at timestamptz
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id, org_id);

ALTER TABLE workspaces ADD UNIQUE (id, org_id);

CREATE TABLE state_versions (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  serial bigint NOT NULL CHECK (serial >= 0),
  lineage text NOT NULL,
  sha256 bytea NOT NULL,
  size integer NOT NULL,
  resources integer NOT NULL,
  blob_key text NOT NULL,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  rolled_back_from uuid REFERENCES state_versions (id),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  UNIQUE (workspace_id, serial)
);

CREATE TABLE state_locks (
  workspace_id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  lock_id text NOT NULL,
  info jsonb NOT NULL,
  locked_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE
);

ALTER TABLE state_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE state_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY state_versions_org ON state_versions USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

ALTER TABLE state_locks ENABLE ROW LEVEL SECURITY;
ALTER TABLE state_locks FORCE ROW LEVEL SECURITY;
CREATE POLICY state_locks_org ON state_locks USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

GRANT SELECT, INSERT, UPDATE ON api_tokens TO stackwright_app;
GRANT SELECT, INSERT ON state_versions TO stackwright_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON state_locks TO stackwright_app;

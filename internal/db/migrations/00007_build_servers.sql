-- +goose Up

-- org_id is NULL for the built-in build server, which serves every org.
CREATE TABLE build_servers (
  id uuid PRIMARY KEY,
  org_id uuid REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  labels jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
  slots integer NOT NULL CHECK (slots BETWEEN 1 AND 64),
  draining boolean NOT NULL DEFAULT false,
  enrolled_via text NOT NULL CHECK (enrolled_via IN ('builtin', 'command', 'ssh')),
  ssh_host text NOT NULL DEFAULT '',
  ssh_port integer NOT NULL DEFAULT 0,
  ssh_user text NOT NULL DEFAULT '',
  ssh_host_key text NOT NULL DEFAULT '',
  platform text NOT NULL DEFAULT '',
  backend text NOT NULL DEFAULT '',
  version text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz,
  removed_at timestamptz,
  CHECK ((org_id IS NULL) = (enrolled_via = 'builtin'))
);
CREATE UNIQUE INDEX build_servers_name_key ON build_servers (org_id, name) NULLS NOT DISTINCT WHERE removed_at IS NULL;

ALTER TABLE build_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_servers FORCE ROW LEVEL SECURITY;
CREATE POLICY build_servers_read ON build_servers FOR SELECT USING (org_id IS NULL OR org_id = app_org_id());
CREATE POLICY build_servers_insert ON build_servers FOR INSERT WITH CHECK (org_id IS NOT DISTINCT FROM app_org_id());
CREATE POLICY build_servers_update ON build_servers FOR UPDATE
  USING (org_id IS NOT DISTINCT FROM app_org_id()) WITH CHECK (org_id IS NOT DISTINCT FROM app_org_id());
GRANT SELECT, INSERT, UPDATE ON build_servers TO stackwright_app;

-- Client certificates of build servers. The builder port looks a
-- certificate up by its serial before it knows the org, so like job_leases
-- this table holds only ids and has no row-level security.
CREATE TABLE build_server_certs (
  serial text PRIMARY KEY,
  build_server_id uuid NOT NULL REFERENCES build_servers (id) ON DELETE CASCADE,
  org_id uuid REFERENCES orgs (id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz
);
CREATE INDEX build_server_certs_server_idx ON build_server_certs (build_server_id);
GRANT SELECT, INSERT, UPDATE ON build_server_certs TO stackwright_app;

-- One-time tokens that enrol a build server into an org. Looked up by hash,
-- with no row-level security.
CREATE TABLE enrol_tokens (
  token_hash bytea PRIMARY KEY,
  build_server_id uuid NOT NULL UNIQUE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL,
  labels jsonb NOT NULL DEFAULT '{}',
  slots integer NOT NULL,
  via text NOT NULL CHECK (via IN ('command', 'ssh')),
  ssh_host text NOT NULL DEFAULT '',
  ssh_port integer NOT NULL DEFAULT 0,
  ssh_user text NOT NULL DEFAULT '',
  ssh_host_key text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at timestamptz
);
GRANT SELECT, INSERT, UPDATE ON enrol_tokens TO stackwright_app;

-- The CA that signs build server certificates and the builder port's
-- certificate. key is sealed with the instance data key.
CREATE TABLE builder_ca (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  cert bytea NOT NULL,
  key bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON builder_ca TO stackwright_app;

-- The key the server signs in with to enrol a build server over SSH.
-- private_key is sealed with the org's data key.
CREATE TABLE org_ssh_keys (
  org_id uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
  public_key text NOT NULL,
  private_key bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE org_ssh_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_ssh_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY org_ssh_keys_org ON org_ssh_keys USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON org_ssh_keys TO stackwright_app;

ALTER TABLE workspaces ADD COLUMN required_labels jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(required_labels) = 'object');
-- Providers from the workspace's last lock file, such as
-- "registry.opentofu.org/hashicorp/random 3.6.0". Builders cache them.
ALTER TABLE workspaces ADD COLUMN providers text[] NOT NULL DEFAULT '{}';
ALTER TABLE jobs ADD COLUMN build_server_id uuid REFERENCES build_servers (id);

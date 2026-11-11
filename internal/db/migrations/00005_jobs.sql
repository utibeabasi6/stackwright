-- +goose Up

CREATE TABLE engines (
  engine text NOT NULL CHECK (engine IN ('opentofu', 'terraform')),
  version text NOT NULL,
  platform text NOT NULL,
  sha256 text NOT NULL,
  size bigint NOT NULL,
  object_key text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (engine, version, platform)
);

CREATE TABLE jobs (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  operation text NOT NULL CHECK (operation IN ('plan', 'apply')),
  plan_job_id uuid REFERENCES jobs (id),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'lost')),
  engine text NOT NULL,
  engine_version text NOT NULL,
  platform text NOT NULL,
  artifact_key bytea NOT NULL,
  has_changes boolean,
  error text,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  deadline timestamptz NOT NULL,
  claimed_at timestamptz,
  finished_at timestamptz,
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  FOREIGN KEY (engine, engine_version, platform) REFERENCES engines (engine, version, platform),
  CHECK ((operation = 'apply') = (plan_job_id IS NOT NULL))
);
CREATE INDEX jobs_workspace_idx ON jobs (workspace_id, created_at DESC);

-- A lease is a live job's credentials. It goes when the job ends. Like
-- api_tokens it has no row-level security: a lookup needs the secret's hash.
CREATE TABLE job_leases (
  job_id uuid PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL,
  ticket_hash bytea UNIQUE,
  token_hash bytea UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  heartbeat_at timestamptz
);

CREATE TABLE job_events (
  job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
  org_id uuid NOT NULL,
  seq bigint NOT NULL CHECK (seq > 0),
  type text NOT NULL,
  resource_addr text NOT NULL DEFAULT '',
  payload jsonb NOT NULL,
  at timestamptz NOT NULL,
  PRIMARY KEY (job_id, seq)
);

ALTER TABLE audit_log DROP CONSTRAINT audit_log_actor_kind_check;
ALTER TABLE audit_log ADD CHECK (actor_kind IN ('user', 'system', 'job'));

ALTER TABLE state_locks ADD COLUMN job_id uuid REFERENCES jobs (id) ON DELETE SET NULL;

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_org ON jobs USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

ALTER TABLE job_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_events FORCE ROW LEVEL SECURITY;
CREATE POLICY job_events_org ON job_events USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

GRANT SELECT, INSERT ON engines TO stackwright_app;
GRANT SELECT, INSERT, UPDATE ON jobs TO stackwright_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON job_leases TO stackwright_app;
GRANT SELECT, INSERT ON job_events TO stackwright_app;

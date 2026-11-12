-- +goose Up

ALTER TABLE workspaces ADD COLUMN auto_apply boolean NOT NULL DEFAULT false;
ALTER TABLE workspaces ADD COLUMN runs_started integer NOT NULL DEFAULT 0;

CREATE TABLE runs (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  number integer NOT NULL,
  status text NOT NULL DEFAULT 'queued' CHECK (status IN
    ('queued', 'planning', 'needs_approval', 'applying', 'planned', 'applied', 'failed', 'rejected', 'canceled')),
  plan_only boolean NOT NULL,
  source text NOT NULL CHECK (length(source) BETWEEN 1 AND 200),
  source_sha256 text NOT NULL,
  plan_job_id uuid REFERENCES jobs (id),
  apply_job_id uuid REFERENCES jobs (id),
  state_serial bigint,
  has_changes boolean,
  adds integer,
  changes integer,
  destroys integer,
  error text,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  decided_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  last_seq bigint NOT NULL DEFAULT 0,
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  UNIQUE (workspace_id, number)
);
CREATE UNIQUE INDEX runs_one_active ON runs (workspace_id) WHERE status IN ('planning', 'needs_approval', 'applying');
CREATE INDEX runs_queued_idx ON runs (created_at) WHERE status = 'queued';
CREATE INDEX runs_org_status_idx ON runs (org_id, status);

-- One timeline per run. seq comes from runs.last_seq, taken under the run's
-- row lock, so a reader that saw seq n has seen every event before it.
CREATE TABLE run_events (
  run_id uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  org_id uuid NOT NULL,
  seq bigint NOT NULL,
  type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  actor_id uuid REFERENCES users (id) ON DELETE SET NULL,
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, seq)
);

ALTER TABLE jobs ADD COLUMN run_id uuid REFERENCES runs (id);
ALTER TABLE jobs ADD COLUMN source_sha256 text;
ALTER TABLE job_events ADD COLUMN run_seq bigint;
CREATE INDEX job_events_run_seq_idx ON job_events (job_id, run_seq);

ALTER TABLE runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE runs FORCE ROW LEVEL SECURITY;
CREATE POLICY runs_org ON runs USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

ALTER TABLE run_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_events FORCE ROW LEVEL SECURITY;
CREATE POLICY run_events_org ON run_events USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

GRANT SELECT, INSERT, UPDATE ON runs TO stackwright_app;
GRANT SELECT, INSERT ON run_events TO stackwright_app;

-- Runs that are not finished, for the dispatcher, which serves every org.
-- Like job_leases it holds only ids, and has no row-level security.
CREATE TABLE run_queue (
  run_id uuid PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  queued_at timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, DELETE ON run_queue TO stackwright_app;

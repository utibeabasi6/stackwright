-- +goose Up

-- Deployments: several workspaces that apply in order with one approval,
-- and drift checks: refresh-only plans on a schedule (adr/0042).

ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
  CHECK (trigger IN ('ui', 'pull_request', 'push', 'agent', 'schedule', 'deployment', 'drift'));
ALTER TABLE runs ADD COLUMN refresh_only boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN destroy boolean NOT NULL DEFAULT false;
-- The variables a deployment step gets from earlier steps' outputs, as
-- sealed JSON.
ALTER TABLE runs ADD COLUMN inputs bytea;
ALTER TABLE runs ADD CONSTRAINT runs_drift_plan_only CHECK (trigger <> 'drift' OR plan_only AND refresh_only);

CREATE TABLE deployments (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz
);
CREATE UNIQUE INDEX deployments_name ON deployments (org_id, name) WHERE archived_at IS NULL;
ALTER TABLE deployments ENABLE ROW LEVEL SECURITY;
CREATE POLICY deployments_org ON deployments USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON deployments TO stackwright_app;

CREATE TABLE deployment_steps (
  deployment_id uuid NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
  org_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  PRIMARY KEY (deployment_id, workspace_id)
);
ALTER TABLE deployment_steps ENABLE ROW LEVEL SECURITY;
CREATE POLICY deployment_steps_org ON deployment_steps USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, DELETE ON deployment_steps TO stackwright_app;

-- A link orders two steps. With an output, the later step gets the
-- earlier one's output as the variable.
CREATE TABLE deployment_links (
  deployment_id uuid NOT NULL,
  org_id uuid NOT NULL,
  from_workspace_id uuid NOT NULL,
  to_workspace_id uuid NOT NULL,
  output text NOT NULL DEFAULT '' CHECK (output ~ '^([A-Za-z_][A-Za-z0-9_-]{0,63})?$'),
  variable text NOT NULL DEFAULT '' CHECK (variable ~ '^([A-Za-z_][A-Za-z0-9_-]{0,63})?$'),
  CHECK ((output = '') = (variable = '')),
  CHECK (from_workspace_id <> to_workspace_id),
  FOREIGN KEY (deployment_id, from_workspace_id) REFERENCES deployment_steps (deployment_id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (deployment_id, to_workspace_id) REFERENCES deployment_steps (deployment_id, workspace_id) ON DELETE CASCADE,
  PRIMARY KEY (deployment_id, from_workspace_id, to_workspace_id, variable)
);
ALTER TABLE deployment_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY deployment_links_org ON deployment_links USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, DELETE ON deployment_links TO stackwright_app;

CREATE TABLE deployment_runs (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  deployment_id uuid NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
  number integer NOT NULL,
  status text NOT NULL DEFAULT 'planning' CHECK (status IN
    ('planning', 'needs_approval', 'applying', 'applied', 'planned', 'failed', 'rejected', 'canceled')),
  destroy boolean NOT NULL DEFAULT false,
  error text,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  decided_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  UNIQUE (deployment_id, number)
);
CREATE UNIQUE INDEX deployment_runs_one_active ON deployment_runs (deployment_id) WHERE status IN ('planning', 'needs_approval', 'applying');
ALTER TABLE deployment_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY deployment_runs_org ON deployment_runs USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON deployment_runs TO stackwright_app;

ALTER TABLE runs ADD COLUMN deployment_run_id uuid REFERENCES deployment_runs (id) ON DELETE SET NULL;
CREATE INDEX runs_deployment_run_idx ON runs (deployment_run_id) WHERE deployment_run_id IS NOT NULL;

-- The newest outputs of each workspace, from its last apply, sealed.
CREATE TABLE workspace_outputs (
  workspace_id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  outputs bytea NOT NULL,
  run_id uuid REFERENCES runs (id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE
);
ALTER TABLE workspace_outputs ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_outputs_org ON workspace_outputs USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON workspace_outputs TO stackwright_app;

-- The workspaces due a drift check: those that applied at least once and
-- have no drift run since the slot. The scheduler finds them across orgs,
-- before any org is set. It returns only ids.
-- +goose StatementBegin
CREATE FUNCTION drift_checks_due(slot timestamptz) RETURNS TABLE (workspace_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT w.id, w.org_id FROM workspaces w JOIN projects p ON p.id = w.project_id
  WHERE w.archived_at IS NULL AND p.archived_at IS NULL
    AND EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.status = 'applied')
    AND NOT EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.trigger = 'drift' AND r.created_at >= slot)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION drift_checks_due(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION drift_checks_due(timestamptz) TO stackwright_app;

-- The deployment runs that the orchestrator advances, across orgs. It
-- returns only ids.
-- +goose StatementBegin
CREATE FUNCTION active_deployment_runs() RETURNS TABLE (deployment_run_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT id, org_id FROM deployment_runs WHERE status IN ('planning', 'applying')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION active_deployment_runs() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION active_deployment_runs() TO stackwright_app;

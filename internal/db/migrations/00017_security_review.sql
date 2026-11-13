-- +goose Up

-- A blocked run waits for an accepted risk, and holds its workspace like
-- one that waits for approval (adr/0037). Nightly plans have their own
-- trigger.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN
  ('queued', 'planning', 'needs_approval', 'blocked', 'applying', 'planned', 'applied', 'failed', 'rejected', 'canceled'));
DROP INDEX runs_one_active;
CREATE UNIQUE INDEX runs_one_active ON runs (workspace_id) WHERE status IN ('planning', 'needs_approval', 'blocked', 'applying');
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check CHECK (trigger IN ('ui', 'pull_request', 'push', 'agent', 'schedule'));

-- One finding for each workspace, check and resource address: its
-- fingerprint.
CREATE TABLE findings (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
  check_id text NOT NULL,
  resource text NOT NULL,
  severity text NOT NULL CHECK (severity IN ('low', 'medium', 'high', 'critical')),
  title text NOT NULL,
  message text NOT NULL,
  fixed_at timestamptz,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, check_id, resource)
);
CREATE INDEX findings_org_idx ON findings (org_id);
ALTER TABLE findings ENABLE ROW LEVEL SECURITY;
CREATE POLICY findings_org ON findings USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON findings TO stackwright_app;

CREATE TABLE risk_acceptances (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  finding_id uuid NOT NULL REFERENCES findings (id) ON DELETE CASCADE,
  accepted_by uuid REFERENCES users (id) ON DELETE SET NULL,
  reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
  accepted_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  CHECK (expires_at > accepted_at AND expires_at <= accepted_at + interval '90 days')
);
CREATE INDEX risk_acceptances_finding_idx ON risk_acceptances (finding_id, expires_at);
ALTER TABLE risk_acceptances ENABLE ROW LEVEL SECURITY;
CREATE POLICY risk_acceptances_org ON risk_acceptances USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON risk_acceptances TO stackwright_app;

-- An org's Rego policy sets, run as Trivy custom checks, and the built-in
-- checks it skips.
CREATE TABLE policy_sets (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  rego text NOT NULL CHECK (length(rego) BETWEEN 1 AND 262144),
  updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, name)
);
ALTER TABLE policy_sets ENABLE ROW LEVEL SECURITY;
CREATE POLICY policy_sets_org ON policy_sets USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON policy_sets TO stackwright_app;

CREATE TABLE skipped_checks (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  check_id text NOT NULL CHECK (check_id ~ '^[A-Za-z0-9._-]{1,64}$'),
  PRIMARY KEY (org_id, check_id)
);
ALTER TABLE skipped_checks ENABLE ROW LEVEL SECURITY;
CREATE POLICY skipped_checks_org ON skipped_checks USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, DELETE ON skipped_checks TO stackwright_app;

-- The workspaces due a nightly review: those whose own code ran before,
-- or whose project has a repository, and that have no scheduled run since
-- the slot. The scheduler finds them across orgs, before any org is set.
-- It returns only ids.
-- +goose StatementBegin
CREATE FUNCTION nightly_reviews_due(slot timestamptz) RETURNS TABLE (workspace_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT w.id, w.org_id FROM workspaces w JOIN projects p ON p.id = w.project_id
  WHERE w.archived_at IS NULL AND p.archived_at IS NULL
    AND (p.repo <> '' OR EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.trigger IN ('ui', 'push', 'schedule')))
    AND NOT EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.trigger = 'schedule' AND r.created_at >= slot)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION nightly_reviews_due(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION nightly_reviews_due(timestamptz) TO stackwright_app;

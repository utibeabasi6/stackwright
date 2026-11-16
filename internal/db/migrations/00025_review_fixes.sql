-- +goose Up

-- Fixes from the review of 2026-11-15 (adr/0047).

-- Only pushes and pull requests run once per commit. Drift checks, nightly
-- reviews and deployments plan the same commit again.
DROP INDEX runs_github_once;
CREATE UNIQUE INDEX runs_github_once ON runs (workspace_id, trigger, commit_sha, coalesce(pr_number, 0))
  WHERE trigger IN ('push', 'pull_request');

-- Org settings, such as the audit retention, change through the API.
CREATE POLICY orgs_update ON orgs FOR UPDATE USING (id = app_org_id()) WITH CHECK (id = app_org_id());

-- The same triggers as runs.OwnCode.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION nightly_reviews_due(slot timestamptz) RETURNS TABLE (workspace_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT w.id, w.org_id FROM workspaces w JOIN projects p ON p.id = w.project_id
  WHERE w.archived_at IS NULL AND p.archived_at IS NULL
    AND (p.repo <> '' OR EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.trigger IN ('ui', 'push', 'schedule', 'deployment')))
    AND NOT EXISTS (SELECT FROM runs r WHERE r.workspace_id = w.id AND r.trigger = 'schedule' AND r.created_at >= slot)
$$;
-- +goose StatementEnd

CREATE INDEX jobs_run_idx ON jobs (run_id);
CREATE INDEX notification_deliveries_destination_idx ON notification_deliveries (destination_id);

-- Whether this org's SCIM made the account, so it may rename it and end
-- all its sign-ins. Accounts linked before this are taken as not made.
ALTER TABLE scim_users ADD COLUMN created_user boolean NOT NULL DEFAULT false;

-- The jobs and toolboxes the hub gave each build server. A builder's hello
-- lists its running jobs; only these count, so a builder cannot end another
-- builder's job. Read before any org is known, it holds only ids, as
-- job_leases does.
CREATE TABLE build_server_jobs (
  job_id text PRIMARY KEY,
  build_server_id uuid NOT NULL REFERENCES build_servers (id) ON DELETE CASCADE,
  started_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON build_server_jobs TO stackwright_app;

-- The checks loop deletes a run's check once its last status is synced.
GRANT DELETE ON github_checks TO stackwright_app;
DELETE FROM github_checks c USING runs r
  WHERE r.id = c.run_id AND r.status IN ('applied', 'planned', 'failed', 'rejected', 'canceled') AND c.synced_status = r.status;

-- Job artifacts go after jobs.KeepArtifacts days (adr/0047). The sweep
-- finds them across orgs, before any org is set, and gets only ids. Kept:
-- the jobs of a run that has not ended, of each workspace's last applied
-- run, and of its last succeeded plan of its own code (runs.OwnCode).
ALTER TABLE jobs ADD COLUMN artifacts_deleted_at timestamptz;
CREATE INDEX jobs_artifacts_idx ON jobs (created_at) WHERE artifacts_deleted_at IS NULL;
-- +goose StatementBegin
CREATE FUNCTION job_artifacts_due(cutoff timestamptz) RETURNS TABLE (job_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  WITH keep AS (
    SELECT plan_job_id AS id FROM runs WHERE status IN ('queued', 'planning', 'needs_approval', 'blocked', 'applying')
    UNION SELECT apply_job_id FROM runs WHERE status IN ('planning', 'needs_approval', 'blocked', 'applying')
    UNION SELECT k.id FROM (SELECT DISTINCT ON (workspace_id) plan_job_id, apply_job_id FROM runs
        WHERE status = 'applied' ORDER BY workspace_id, number DESC) a,
      LATERAL (VALUES (a.plan_job_id), (a.apply_job_id)) k (id)
    UNION SELECT p.plan_job_id FROM (SELECT DISTINCT ON (r.workspace_id) r.plan_job_id FROM runs r JOIN jobs j ON j.id = r.plan_job_id
        WHERE r.trigger IN ('ui', 'push', 'schedule', 'deployment') AND j.status = 'succeeded'
        ORDER BY r.workspace_id, r.number DESC) p
  )
  SELECT j.id, j.org_id FROM jobs j
  WHERE j.created_at < cutoff AND j.artifacts_deleted_at IS NULL AND j.status NOT IN ('queued', 'running')
    AND NOT EXISTS (SELECT FROM keep WHERE keep.id = j.id)
  ORDER BY j.created_at LIMIT 500
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION job_artifacts_due(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION job_artifacts_due(timestamptz) TO stackwright_app;

-- The engine's token, which reaches only its job's state (adr/0047).
ALTER TABLE job_leases ADD COLUMN state_token_hash bytea UNIQUE;

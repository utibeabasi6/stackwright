-- +goose Up

-- The instance's GitHub App, made from a manifest. Secrets are sealed with
-- the instance data key.
CREATE TABLE github_app (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  app_id bigint NOT NULL,
  slug text NOT NULL,
  name text NOT NULL,
  html_url text NOT NULL,
  owner text NOT NULL,
  client_id text NOT NULL,
  client_secret bytea NOT NULL,
  webhook_secret bytea NOT NULL,
  private_key bytea NOT NULL,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, DELETE ON github_app TO stackwright_app;

-- One-time states for the browser's trips to GitHub: making the App,
-- installing it, and the installer's authorization. Looked up by hash.
CREATE TABLE github_states (
  state_hash bytea PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('manifest', 'install', 'authorize')),
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  org_id uuid REFERENCES orgs (id) ON DELETE CASCADE,
  installation_id bigint,
  -- The Stackwright page to come back to, such as /acme/settings/github.
  return_to text NOT NULL CHECK (return_to ~ '^/[^/]'),
  expires_at timestamptz NOT NULL
);
GRANT SELECT, INSERT, DELETE ON github_states TO stackwright_app;

-- A webhook names an installation before anyone knows the org, so this
-- table has no row-level security. It holds ids and the account's login.
CREATE TABLE github_installations (
  installation_id bigint PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  account text NOT NULL,
  linked_by uuid REFERENCES users (id) ON DELETE SET NULL,
  linked_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX github_installations_org_idx ON github_installations (org_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON github_installations TO stackwright_app;

CREATE TABLE repos (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  installation_id bigint NOT NULL REFERENCES github_installations (installation_id) ON DELETE CASCADE,
  github_id bigint NOT NULL,
  full_name text NOT NULL,
  default_branch text NOT NULL,
  private boolean NOT NULL,
  UNIQUE (org_id, github_id)
);
CREATE INDEX repos_full_name_idx ON repos (org_id, lower(full_name));
ALTER TABLE repos ENABLE ROW LEVEL SECURITY;
ALTER TABLE repos FORCE ROW LEVEL SECURITY;
CREATE POLICY repos_org ON repos USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON repos TO stackwright_app;

-- A project's repository as "owner/name", from the URL typed in phase 2
-- (adr/0016), so a webhook finds its projects.
ALTER TABLE projects ADD COLUMN repo text GENERATED ALWAYS AS
  (lower(regexp_replace(trim(repo_url), '^(https?://)?(www\.)?github\.com/|\.git$|/+$', '', 'g'))) STORED;
CREATE INDEX projects_repo_idx ON projects (org_id, repo) WHERE archived_at IS NULL;

-- Stored before any work, so a redelivery with the same id is stored once.
CREATE TABLE webhook_deliveries (
  delivery_id text PRIMARY KEY,
  event text NOT NULL,
  payload jsonb NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  error text
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE processed_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_deliveries TO stackwright_app;

-- One sealed copy of each commit that runs, shared by its runs.
CREATE TABLE source_archives (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
  commit_sha text NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
  object_key text NOT NULL,
  sha256 text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, commit_sha)
);
ALTER TABLE source_archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_archives FORCE ROW LEVEL SECURITY;
CREATE POLICY source_archives_org ON source_archives USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON source_archives TO stackwright_app;

ALTER TABLE runs ADD COLUMN trigger text NOT NULL DEFAULT 'ui' CHECK (trigger IN ('ui', 'pull_request', 'push'));
ALTER TABLE runs ADD COLUMN commit_sha text;
ALTER TABLE runs ADD COLUMN pr_number integer;
ALTER TABLE runs ADD COLUMN branch text;
ALTER TABLE runs ADD COLUMN repo_id uuid REFERENCES repos (id) ON DELETE SET NULL;
-- An archive that runs share; NULL means the run's own copy.
ALTER TABLE runs ADD COLUMN source_key text;
CREATE UNIQUE INDEX runs_github_once ON runs (workspace_id, trigger, commit_sha, coalesce(pr_number, 0)) WHERE trigger <> 'ui';
CREATE INDEX runs_pull_idx ON runs (repo_id, pr_number) WHERE pr_number IS NOT NULL;

-- What GitHub was last told about a run. The sync loop serves every org, so
-- like run_queue this table holds ids and no row-level security.
CREATE TABLE github_checks (
  run_id uuid PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  check_run_id bigint,
  synced_status text NOT NULL DEFAULT '',
  attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX github_checks_org_idx ON github_checks (org_id);
GRANT SELECT, INSERT, UPDATE ON github_checks TO stackwright_app;

CREATE TABLE github_comments (
  repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
  pr_number integer NOT NULL,
  org_id uuid NOT NULL,
  comment_id bigint NOT NULL,
  PRIMARY KEY (repo_id, pr_number)
);
ALTER TABLE github_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_comments FORCE ROW LEVEL SECURITY;
CREATE POLICY github_comments_org ON github_comments USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON github_comments TO stackwright_app;

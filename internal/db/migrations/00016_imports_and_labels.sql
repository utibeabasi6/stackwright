-- +goose Up

-- Labels on projects (adr/0035), and where a project's state lives
-- (adr/0036).
ALTER TABLE projects
  ADD COLUMN labels jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN state_backend text NOT NULL DEFAULT 'stackwright' CHECK (state_backend IN ('stackwright', 'code', 'git'));

-- State committed to Git is plan-only: no run of such a project may apply,
-- and no apply job may follow its plans.
-- +goose StatementBegin
CREATE FUNCTION git_state(workspace uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT EXISTS (SELECT FROM workspaces w JOIN projects p ON p.id = w.project_id WHERE w.id = workspace AND p.state_backend = 'git')
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION refuse_git_state_run() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT NEW.plan_only AND git_state(NEW.workspace_id) THEN
    RAISE EXCEPTION 'a project whose state is committed to Git only plans' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION refuse_git_state_apply() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.operation = 'apply' AND git_state(NEW.workspace_id) THEN
    RAISE EXCEPTION 'a project whose state is committed to Git only plans' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_refuse_git_state_apply BEFORE INSERT OR UPDATE OF plan_only ON runs
  FOR EACH ROW EXECUTE FUNCTION refuse_git_state_run();
CREATE TRIGGER jobs_refuse_git_state_apply BEFORE INSERT ON jobs
  FOR EACH ROW EXECUTE FUNCTION refuse_git_state_apply();

CREATE TABLE imports (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
  commit_sha text NOT NULL,
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE imports ENABLE ROW LEVEL SECURITY;
CREATE POLICY imports_org ON imports USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON imports TO stackwright_app;

CREATE TABLE import_workspaces (
  import_id uuid NOT NULL REFERENCES imports (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
  created boolean NOT NULL,
  run_id uuid REFERENCES runs (id) ON DELETE SET NULL,
  PRIMARY KEY (import_id, workspace_id)
);
ALTER TABLE import_workspaces ENABLE ROW LEVEL SECURITY;
CREATE POLICY import_workspaces_org ON import_workspaces USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON import_workspaces TO stackwright_app;

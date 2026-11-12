-- +goose Up

-- An agent plan is a run with trigger 'agent'. It is speculative: it is
-- always plan-only, and no apply job may follow it (adr/0031).
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check CHECK (trigger IN ('ui', 'pull_request', 'push', 'agent'));
ALTER TABLE runs ADD COLUMN agent_session_id uuid REFERENCES agent_sessions (id) ON DELETE SET NULL;
ALTER TABLE runs ADD CONSTRAINT runs_agent_plan_only CHECK (trigger <> 'agent' OR plan_only AND status NOT IN ('needs_approval', 'applying', 'applied'));

-- +goose StatementBegin
CREATE FUNCTION refuse_agent_apply() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.operation = 'apply' AND EXISTS (SELECT FROM runs WHERE plan_job_id = NEW.plan_job_id AND trigger = 'agent') THEN
    RAISE EXCEPTION 'an agent plan is speculative and is never applied' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER jobs_refuse_agent_apply BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION refuse_agent_apply();

-- The pull request that a person opened from the session's branch.
ALTER TABLE agent_sessions ADD COLUMN pr_number int, ADD COLUMN pr_url text;

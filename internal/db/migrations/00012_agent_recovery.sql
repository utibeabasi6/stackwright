-- +goose Up

-- The sessions that a server process left busy. A new process runs no turn
-- yet, so it finds them across orgs, before any org is set, and ends their
-- turns. It returns only ids.
-- +goose StatementBegin
CREATE FUNCTION agent_busy_sessions() RETURNS TABLE (session_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT id, org_id FROM agent_sessions WHERE status IN ('working', 'starting')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION agent_busy_sessions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION agent_busy_sessions() TO stackwright_app;

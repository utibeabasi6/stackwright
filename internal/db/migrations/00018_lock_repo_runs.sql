-- +goose Up

-- Locks the runs of an installation's repositories before a delete takes
-- the repositories' rows. The dispatcher locks a run and then, as Postgres
-- checks runs.repo_id again, its repository; a delete that took the
-- repository first deadlocked with it. With no installation, every
-- repository counts; keep spares those with these GitHub ids. It reads
-- across orgs, as the cascade does, and returns nothing.
-- +goose StatementBegin
CREATE FUNCTION lock_repo_runs(installation bigint, keep bigint[] DEFAULT NULL) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
  PERFORM 1 FROM runs WHERE repo_id IN (
    SELECT id FROM repos WHERE (installation IS NULL OR installation_id = installation)
      AND (keep IS NULL OR NOT github_id = ANY (keep))
  ) ORDER BY id FOR UPDATE;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION lock_repo_runs(bigint, bigint[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lock_repo_runs(bigint, bigint[]) TO stackwright_app;

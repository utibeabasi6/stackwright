-- +goose Up

-- Where a session's sandbox runs, so a server that restarts can take the
-- sandbox back when its toolbox reconnects.
ALTER TABLE agent_sessions ADD COLUMN sandbox_platform text, ADD COLUMN sandbox_remote boolean NOT NULL DEFAULT false;

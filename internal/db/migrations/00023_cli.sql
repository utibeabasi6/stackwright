-- +goose Up

-- The CLI: device codes, the tokens they give, and plans from the CLI,
-- which never apply (adr/0044).

CREATE TABLE device_codes (
  device_hash bytea PRIMARY KEY,
  user_code text NOT NULL UNIQUE,
  -- The machine the CLI runs on, as it says.
  name text NOT NULL CHECK (length(name) <= 100),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  polled_at timestamptz,
  approved_by uuid REFERENCES users (id) ON DELETE CASCADE,
  denied boolean NOT NULL DEFAULT false
);
GRANT SELECT, INSERT, UPDATE, DELETE ON device_codes TO stackwright_app;

CREATE TABLE cli_tokens (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  name text NOT NULL CHECK (length(name) <= 100),
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);
CREATE INDEX cli_tokens_user_idx ON cli_tokens (user_id);
GRANT SELECT, INSERT, UPDATE ON cli_tokens TO stackwright_app;

ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
  CHECK (trigger IN ('ui', 'pull_request', 'push', 'agent', 'schedule', 'deployment', 'drift', 'cli'));
ALTER TABLE runs ADD CONSTRAINT runs_cli_plan_only CHECK (trigger <> 'cli' OR plan_only);

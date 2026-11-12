-- +goose Up

-- An org's agent settings. The OpenRouter key is sealed with the org's data
-- key. The cap is US dollars for each calendar month, in UTC.
CREATE TABLE agent_settings (
  org_id uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
  api_key_sealed bytea,
  turn_model text NOT NULL DEFAULT 'anthropic/claude-opus-5.5' CHECK (turn_model ~ '^[A-Za-z0-9._:/-]{1,200}$'),
  short_model text NOT NULL DEFAULT 'anthropic/claude-haiku-5.5' CHECK (short_model ~ '^[A-Za-z0-9._:/-]{1,200}$'),
  monthly_cap numeric(12, 2) NOT NULL DEFAULT 50 CHECK (monthly_cap >= 0),
  updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE agent_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_settings_org ON agent_settings USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON agent_settings TO stackwright_app;

CREATE TABLE agent_sessions (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL,
  repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
  branch text NOT NULL,
  base text NOT NULL,
  status text NOT NULL CHECK (status IN ('starting', 'ready', 'working', 'waiting_for_answer', 'waiting_for_approval',
    'asleep', 'closed', 'failed')),
  title text NOT NULL DEFAULT '' CHECK (length(title) <= 200),
  dirty boolean NOT NULL DEFAULT false,
  error text,
  last_seq bigint NOT NULL DEFAULT 0,
  -- The hub's id for the session's sandbox, while one runs.
  sandbox_id text,
  idle_since timestamptz NOT NULL DEFAULT now(),
  created_by uuid REFERENCES users (id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE,
  UNIQUE (id, org_id)
);
CREATE INDEX agent_sessions_workspace_idx ON agent_sessions (workspace_id, created_at DESC);
ALTER TABLE agent_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_sessions_org ON agent_sessions USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON agent_sessions TO stackwright_app;

-- A session's transcript. cost is what the model call behind the step cost.
CREATE TABLE agent_steps (
  session_id uuid NOT NULL,
  org_id uuid NOT NULL,
  seq bigint NOT NULL CHECK (seq > 0),
  kind text NOT NULL CHECK (kind IN ('user', 'assistant', 'tool_call', 'tool_result', 'proposal', 'decision', 'notice', 'error')),
  payload jsonb NOT NULL DEFAULT '{}',
  cost numeric(12, 6) NOT NULL DEFAULT 0 CHECK (cost >= 0),
  actor_id uuid REFERENCES users (id) ON DELETE SET NULL,
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id, org_id) REFERENCES agent_sessions (id, org_id) ON DELETE CASCADE
);
CREATE INDEX agent_steps_spend_idx ON agent_steps (org_id, at) WHERE cost > 0;
ALTER TABLE agent_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_steps_org ON agent_steps USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT ON agent_steps TO stackwright_app;

-- A person's approval of a proposed commit. A push through the Git proxy
-- spends one: it sets used_at.
CREATE TABLE agent_approvals (
  session_id uuid NOT NULL,
  org_id uuid NOT NULL,
  proposal_seq bigint NOT NULL,
  approved_by uuid NOT NULL REFERENCES users (id),
  approved_at timestamptz NOT NULL DEFAULT now(),
  used_at timestamptz,
  PRIMARY KEY (session_id, proposal_seq),
  FOREIGN KEY (session_id, org_id) REFERENCES agent_sessions (id, org_id) ON DELETE CASCADE
);
ALTER TABLE agent_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_approvals_org ON agent_approvals USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON agent_approvals TO stackwright_app;

-- A running sandbox's token, which its toolbox and its Git client present.
-- It is read before any org is known, as job_leases is, so it has no row
-- security; it holds only hashes and ids.
CREATE TABLE agent_sandbox_tokens (
  token_hash bytea PRIMARY KEY,
  session_id uuid NOT NULL REFERENCES agent_sessions (id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, DELETE ON agent_sandbox_tokens TO stackwright_app;

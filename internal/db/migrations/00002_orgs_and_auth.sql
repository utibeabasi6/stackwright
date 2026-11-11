-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'stackwright_app') THEN
    CREATE ROLE stackwright_app NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
  END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO stackwright_app;

CREATE TYPE member_role AS ENUM ('viewer', 'security', 'reviewer', 'writer', 'admin', 'owner');

CREATE FUNCTION app_user_id() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT nullif(current_setting('stackwright.user_id', true), '')::uuid $$;
CREATE FUNCTION app_org_id() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT nullif(current_setting('stackwright.org_id', true), '')::uuid $$;
CREATE FUNCTION app_invite_token_hash() RETURNS bytea LANGUAGE sql STABLE AS
  $$ SELECT decode(nullif(current_setting('stackwright.invite_token_hash', true), ''), 'hex') $$;

CREATE TABLE data_keys (
  id uuid PRIMARY KEY,
  org_id uuid,
  kek_id bytea NOT NULL,
  wrapped bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX data_keys_scope_key ON data_keys (org_id) NULLS NOT DISTINCT;

CREATE TABLE users (
  id uuid PRIMARY KEY,
  email text NOT NULL CHECK (email ~ '^[^@\s]+@[^@\s]+$'),
  name text NOT NULL DEFAULT '' CHECK (length(name) <= 100),
  password_hash text,
  instance_admin boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE sign_in_providers (
  id uuid PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('oidc', 'github')),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
  issuer text NOT NULL DEFAULT '',
  client_id text NOT NULL,
  client_secret bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'oidc') = (issuer <> ''))
);

CREATE TABLE identities (
  provider_id uuid NOT NULL REFERENCES sign_in_providers (id) ON DELETE CASCADE,
  subject text NOT NULL,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider_id, subject)
);

CREATE TABLE sessions (
  id_hash bytea PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  csrf_token text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE orgs (
  id uuid PRIMARY KEY,
  slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  role member_role NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

CREATE TABLE invites (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  email text NOT NULL CHECK (email ~ '^[^@\s]+@[^@\s]+$'),
  role member_role NOT NULL,
  token_hash bytea NOT NULL UNIQUE,
  invited_by uuid NOT NULL REFERENCES users (id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  accepted_at timestamptz,
  revoked_at timestamptz
);
CREATE UNIQUE INDEX invites_open_email_key ON invites (org_id, lower(email))
  WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id uuid REFERENCES orgs (id) ON DELETE CASCADE,
  actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'system')),
  actor_id text NOT NULL,
  action text NOT NULL,
  target text NOT NULL,
  ip inet,
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_org_at_idx ON audit_log (org_id, at DESC);

ALTER TABLE orgs ENABLE ROW LEVEL SECURITY;
ALTER TABLE orgs FORCE ROW LEVEL SECURITY;
CREATE POLICY orgs_select ON orgs FOR SELECT USING (
  id = app_org_id() OR id IN (SELECT org_id FROM memberships WHERE user_id = app_user_id())
);
CREATE POLICY orgs_insert ON orgs FOR INSERT WITH CHECK (id = app_org_id());

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY memberships_select ON memberships FOR SELECT USING (
  org_id = app_org_id() OR user_id = app_user_id()
);
CREATE POLICY memberships_insert ON memberships FOR INSERT WITH CHECK (org_id = app_org_id());
CREATE POLICY memberships_update ON memberships FOR UPDATE
  USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
CREATE POLICY memberships_delete ON memberships FOR DELETE USING (org_id = app_org_id());

ALTER TABLE invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE invites FORCE ROW LEVEL SECURITY;
CREATE POLICY invites_select ON invites FOR SELECT USING (
  org_id = app_org_id() OR token_hash = app_invite_token_hash()
);
CREATE POLICY invites_insert ON invites FOR INSERT WITH CHECK (org_id = app_org_id());
CREATE POLICY invites_update ON invites FOR UPDATE
  USING (org_id = app_org_id() OR token_hash = app_invite_token_hash())
  WITH CHECK (org_id = app_org_id() OR token_hash = app_invite_token_hash());

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_log_select ON audit_log FOR SELECT USING (org_id = app_org_id());
CREATE POLICY audit_log_insert ON audit_log FOR INSERT WITH CHECK (org_id IS NULL OR org_id = app_org_id());

ALTER TABLE data_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE data_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY data_keys_access ON data_keys USING (org_id IS NULL OR org_id = app_org_id())
  WITH CHECK (org_id IS NULL OR org_id = app_org_id());

GRANT SELECT, INSERT, UPDATE, DELETE ON users, sign_in_providers, identities, sessions, orgs, memberships, invites
  TO stackwright_app;
GRANT SELECT, INSERT ON audit_log, data_keys TO stackwright_app;

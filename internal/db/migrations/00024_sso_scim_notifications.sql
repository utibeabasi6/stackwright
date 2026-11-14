-- +goose Up

-- SAML and SCIM for each org, notifications, and audit export (adr/0045).

-- The sign-in path reads these before any org is set, as it reads
-- sessions, so they have no row-level security; the API filters by org.
CREATE TABLE org_saml (
  org_id uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
  metadata_url text NOT NULL DEFAULT '',
  idp_entity_id text NOT NULL,
  sso_url text NOT NULL,
  -- PEM.
  certificates text[] NOT NULL,
  groups_attribute text NOT NULL DEFAULT 'groups',
  default_role member_role NOT NULL DEFAULT 'viewer',
  required boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON org_saml TO stackwright_app;

CREATE TABLE org_group_roles (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  group_name text NOT NULL CHECK (length(group_name) BETWEEN 1 AND 200),
  role member_role NOT NULL,
  PRIMARY KEY (org_id, group_name)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON org_group_roles TO stackwright_app;

-- An AuthnRequest that waits for its response.
CREATE TABLE saml_requests (
  id text PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  next text NOT NULL DEFAULT '',
  expires_at timestamptz NOT NULL
);
GRANT SELECT, INSERT, DELETE ON saml_requests TO stackwright_app;

-- The org whose identity provider signed the holder in. A device code
-- carries its approver's to the CLI token it gives.
ALTER TABLE sessions ADD COLUMN sso_org_id uuid REFERENCES orgs (id) ON DELETE CASCADE;
ALTER TABLE cli_tokens ADD COLUMN sso_org_id uuid REFERENCES orgs (id) ON DELETE CASCADE;
ALTER TABLE device_codes ADD COLUMN sso_org_id uuid REFERENCES orgs (id) ON DELETE CASCADE;

-- The SAML and SCIM endpoints find their org by its slug before any org
-- is set. It returns only the id.
-- +goose StatementBegin
CREATE FUNCTION org_by_slug(org_slug text) RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT id FROM orgs WHERE slug = org_slug
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION org_by_slug(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION org_by_slug(text) TO stackwright_app;

CREATE TABLE scim_tokens (
  org_id uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON scim_tokens TO stackwright_app;

CREATE TABLE scim_users (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  user_name text NOT NULL,
  external_id text NOT NULL DEFAULT '',
  active boolean NOT NULL DEFAULT true,
  UNIQUE (org_id, user_name),
  UNIQUE (org_id, user_id),
  UNIQUE (id, org_id)
);
ALTER TABLE scim_users ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_users_org ON scim_users USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON scim_users TO stackwright_app;

CREATE TABLE scim_groups (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  display_name text NOT NULL,
  external_id text NOT NULL DEFAULT '',
  UNIQUE (org_id, display_name),
  UNIQUE (id, org_id)
);
ALTER TABLE scim_groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_groups_org ON scim_groups USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON scim_groups TO stackwright_app;

CREATE TABLE scim_group_members (
  group_id uuid NOT NULL,
  scim_user_id uuid NOT NULL,
  org_id uuid NOT NULL,
  FOREIGN KEY (group_id, org_id) REFERENCES scim_groups (id, org_id) ON DELETE CASCADE,
  FOREIGN KEY (scim_user_id, org_id) REFERENCES scim_users (id, org_id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, scim_user_id)
);
ALTER TABLE scim_group_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_group_members_org ON scim_group_members USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, DELETE ON scim_group_members TO stackwright_app;

CREATE TABLE notification_destinations (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('slack', 'webhook')),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  -- Sealed with the org's key; a Slack URL is a secret.
  url bytea NOT NULL,
  secret bytea,
  events text[] NOT NULL CHECK (events <@ ARRAY['run_failed', 'run_needs_approval', 'run_applied', 'finding_opened', 'drift_found']),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name),
  UNIQUE (id, org_id)
);
ALTER TABLE notification_destinations ENABLE ROW LEVEL SECURITY;
CREATE POLICY notification_destinations_org ON notification_destinations USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_destinations TO stackwright_app;

CREATE TABLE notification_deliveries (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL,
  destination_id uuid NOT NULL,
  event text NOT NULL,
  payload jsonb NOT NULL,
  attempts integer NOT NULL DEFAULT 0,
  next_at timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  failed_at timestamptz,
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (destination_id, org_id) REFERENCES notification_destinations (id, org_id) ON DELETE CASCADE
);
CREATE INDEX notification_deliveries_due ON notification_deliveries (next_at) WHERE delivered_at IS NULL AND failed_at IS NULL;
ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY notification_deliveries_org ON notification_deliveries USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE ON notification_deliveries TO stackwright_app;

-- The deliveries due now, across orgs. It returns only ids.
-- +goose StatementBegin
CREATE FUNCTION due_notifications() RETURNS TABLE (delivery_id uuid, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT id, org_id FROM notification_deliveries
  WHERE delivered_at IS NULL AND failed_at IS NULL AND next_at <= now() ORDER BY next_at LIMIT 100
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION due_notifications() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION due_notifications() TO stackwright_app;

ALTER TABLE orgs ADD COLUMN audit_retention_days integer NOT NULL DEFAULT 365 CHECK (audit_retention_days BETWEEN 30 AND 3650);

CREATE TABLE audit_exports (
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('s3', 'webhook')),
  bucket text NOT NULL DEFAULT '',
  prefix text NOT NULL DEFAULT '',
  cloud_account_id uuid,
  url bytea,
  secret bytea,
  -- The newest audit row this destination has.
  sent_id bigint NOT NULL DEFAULT 0,
  sent_at timestamptz,
  last_error text,
  PRIMARY KEY (org_id, kind),
  FOREIGN KEY (cloud_account_id, org_id) REFERENCES cloud_accounts (id, org_id),
  CHECK (kind <> 's3' OR bucket <> '' AND cloud_account_id IS NOT NULL),
  CHECK (kind <> 'webhook' OR url IS NOT NULL AND secret IS NOT NULL)
);
ALTER TABLE audit_exports ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_exports_org ON audit_exports USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON audit_exports TO stackwright_app;

-- The orgs with audit rows to export or to delete, across orgs.
-- +goose StatementBegin
CREATE FUNCTION audit_work() RETURNS TABLE (org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT DISTINCT a.org_id FROM audit_log a JOIN orgs o ON o.id = a.org_id
  WHERE a.id > coalesce((SELECT min(sent_id) FROM audit_exports e WHERE e.org_id = a.org_id), a.id)
    OR a.at < now() - make_interval(days => o.audit_retention_days)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION audit_work() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_work() TO stackwright_app;

-- The audit log stays append-only for the app; only this deletes, and
-- only rows past the org's retention that every export has.
-- +goose StatementBegin
CREATE FUNCTION prune_audit_log(org uuid) RETURNS bigint
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
  WITH gone AS (
    DELETE FROM audit_log a USING orgs o WHERE o.id = org AND a.org_id = org
      AND a.at < now() - make_interval(days => o.audit_retention_days)
      AND a.id <= coalesce((SELECT min(sent_id) FROM audit_exports WHERE org_id = org), a.id)
    RETURNING 1
  ) SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION prune_audit_log(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION prune_audit_log(uuid) TO stackwright_app;

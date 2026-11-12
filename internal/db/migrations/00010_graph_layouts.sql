-- +goose Up

-- The plan graph's node positions for a workspace, as {"node id": [x, y]}.
-- A save merges into it; a re-layout replaces it.
CREATE TABLE graph_layouts (
  workspace_id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
  positions jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(positions) = 'object'),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, org_id) REFERENCES workspaces (id, org_id) ON DELETE CASCADE
);

ALTER TABLE graph_layouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE graph_layouts FORCE ROW LEVEL SECURITY;
CREATE POLICY graph_layouts_org ON graph_layouts USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON graph_layouts TO stackwright_app;

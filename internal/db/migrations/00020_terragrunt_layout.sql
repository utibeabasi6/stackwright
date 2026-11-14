-- +goose Up

-- A Terragrunt stack: a folder of units on the backends their
-- remote_state names (adr/0041).
ALTER TABLE projects DROP CONSTRAINT projects_layout_check;
ALTER TABLE projects ADD CONSTRAINT projects_layout_check CHECK (layout IN ('plain', 'cli_workspaces', 'terragrunt'));

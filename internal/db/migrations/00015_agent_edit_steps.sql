-- +goose Up

-- An edit step records a person's save in the editor; the model reads it
-- as a note (adr/0034).
ALTER TABLE agent_steps DROP CONSTRAINT agent_steps_kind_check;
ALTER TABLE agent_steps ADD CONSTRAINT agent_steps_kind_check
  CHECK (kind IN ('user', 'assistant', 'tool_call', 'tool_result', 'proposal', 'decision', 'notice', 'error', 'edit'));

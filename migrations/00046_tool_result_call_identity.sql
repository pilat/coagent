-- +goose Up

ALTER TABLE messages ADD COLUMN tool_call_owner_id INTEGER REFERENCES messages(id);
ALTER TABLE messages ADD COLUMN tool_call_index INTEGER;

CREATE UNIQUE INDEX IF NOT EXISTS messages_tool_call_identity
    ON messages(tool_call_owner_id, tool_call_index)
    WHERE role = 'tool' AND tool_call_owner_id IS NOT NULL;

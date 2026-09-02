-- +goose Up

-- Typed failure bit on tool result rows. Legacy rows read as false, so the
-- provider adapters and the scheduler treat pre-migration results as ordinary
-- output. Append-only history: existing rows are never rewritten.
ALTER TABLE messages ADD COLUMN tool_error BOOLEAN NOT NULL DEFAULT 0;

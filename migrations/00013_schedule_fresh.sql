-- +goose Up
-- fresh=1 schedules reset the session's context on each fire (blank slate + the
-- schedule's own prompt) instead of appending a tick to the accumulated history.
ALTER TABLE schedules ADD COLUMN fresh INTEGER NOT NULL DEFAULT 0;

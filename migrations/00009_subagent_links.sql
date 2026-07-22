-- +goose Up
-- First-class subagents: durable parent↔child links + root_id on sessions.
-- subagent_links is the source of truth for "a completion is owed", surviving
-- compaction, restart, and lost in-memory notifications.

ALTER TABLE sessions ADD COLUMN root_id INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS subagent_links (
    parent_id INTEGER NOT NULL,
    child_id INTEGER NOT NULL,
    task_call_id TEXT NOT NULL,
    blocking INTEGER NOT NULL DEFAULT 0,
    depth INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'spawned',
    delivered_at INTEGER,
    delivered_msg_id INTEGER,
    timeout_sec INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (parent_id, child_id)
);

CREATE INDEX IF NOT EXISTS idx_subagent_links_undelivered
    ON subagent_links(parent_id) WHERE delivered_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_subagent_links_child
    ON subagent_links(child_id);

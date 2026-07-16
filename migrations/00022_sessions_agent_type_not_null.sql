-- +goose NO TRANSACTION
-- +goose Up
-- Drop the agent_type default: an INSERT that forgets the column made roots
-- resume as 'general' subagents. SQLite cannot alter a default, so the table is
-- rebuilt per the documented 12-step ALTER TABLE procedure.
-- The pool always opens connections with foreign_keys=1, so step 1 is unconditional.

PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS sessions_new;
DROP TABLE IF EXISTS sessions_seq_backup;

-- sqlite_sequence loses the high-water mark when the old table is dropped, and
-- subagent_links has no foreign key to catch a reused session id.
CREATE TABLE sessions_seq_backup AS SELECT seq FROM sqlite_sequence WHERE name = 'sessions';

CREATE TABLE sessions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
    model TEXT,
    reasoning_level TEXT NOT NULL DEFAULT 'medium',
    master_enabled BOOLEAN DEFAULT FALSE,
    attributes TEXT DEFAULT '{}',
    agent_type TEXT NOT NULL,
    parent_id INTEGER DEFAULT 0,
    iteration INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    todo_items TEXT DEFAULT '[]',
    compaction_brief TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    killed_at DATETIME,
    root_id INTEGER NOT NULL DEFAULT 0
);

-- A legacy NULL cannot pass the new constraint; it gets 00021's rule.
INSERT INTO sessions_new (
    id, project_id, model, reasoning_level, master_enabled, attributes, agent_type,
    parent_id, iteration, status, todo_items, compaction_brief,
    created_at, updated_at, killed_at, root_id
)
SELECT
    id, project_id, model, reasoning_level, master_enabled, attributes,
    COALESCE(agent_type, CASE WHEN parent_id IS NULL OR parent_id = 0 THEN 'build' ELSE 'general' END),
    parent_id, iteration, status, todo_items, compaction_brief,
    created_at, updated_at, killed_at, root_id
FROM sessions;

DROP TABLE sessions;

ALTER TABLE sessions_new RENAME TO sessions;

CREATE INDEX IF NOT EXISTS idx_sessions_project_id ON sessions(project_id);

DELETE FROM sqlite_sequence WHERE name = 'sessions';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'sessions', MAX(
    COALESCE((SELECT seq FROM sessions_seq_backup), 0),
    COALESCE((SELECT MAX(id) FROM sessions), 0)
);

DROP TABLE sessions_seq_backup;

PRAGMA foreign_keys = ON;

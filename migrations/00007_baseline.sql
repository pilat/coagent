-- +goose Up
-- Baseline schema: snapshot of the full database schema after migrations 1–6.
-- For fresh databases this creates everything from scratch.
-- For existing databases (already at version 6) this is a no-op thanks to
-- CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS.

CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    work_dir TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
    model TEXT,
    reasoning_level TEXT NOT NULL DEFAULT 'medium',
    master_enabled BOOLEAN DEFAULT FALSE,
    attributes TEXT DEFAULT '{}',
    agent_type TEXT DEFAULT 'general',
    parent_id INTEGER DEFAULT 0,
    iteration INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    todo_items TEXT DEFAULT '[]',
    compaction_brief TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    killed_at DATETIME
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL,
    content TEXT,
    tool_call_id TEXT,
    tool_name TEXT,
    tool_calls TEXT,
    reasoning_content TEXT,
    cost_usd REAL DEFAULT 0,
    usage TEXT,
    compacted_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_messages_session_active
    ON messages(session_id) WHERE compacted_at IS NULL;

CREATE TABLE IF NOT EXISTS schedules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    cron_expr TEXT,
    one_shot_at DATETIME,
    input_message TEXT NOT NULL,
    last_fired_at DATETIME,
    metadata TEXT DEFAULT '{}',
    fire_count INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_trigger CHECK (cron_expr IS NOT NULL OR one_shot_at IS NOT NULL)
);

CREATE TABLE IF NOT EXISTS memories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS extractions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL DEFAULT 0 REFERENCES projects(id),
    session_id INTEGER NOT NULL DEFAULT 0 REFERENCES sessions(id),
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS extraction_chunks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    extraction_id INTEGER NOT NULL REFERENCES extractions(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    text TEXT NOT NULL,
    embedding BLOB NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(extraction_id, chunk_index)
);

CREATE TABLE IF NOT EXISTS memory_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_project_id ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_extractions_project_id ON extractions(project_id);
CREATE INDEX IF NOT EXISTS idx_memories_project_id ON memories(project_id);

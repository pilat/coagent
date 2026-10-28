-- +goose Up

CREATE UNIQUE INDEX idx_sessions_assistant_root
    ON sessions(json_extract(attributes, '$.manager_id'))
    WHERE agent_type = 'assistant' AND parent_id = 0;

CREATE TABLE assistant_memory (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    text TEXT NOT NULL,
    until TEXT CHECK (until IS NULL OR until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE INDEX idx_assistant_memory_session ON assistant_memory(session_id, id);

CREATE TABLE assistant_focus (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    text TEXT NOT NULL,
    sessions TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(sessions) AND json_type(sessions) = 'array'),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE INDEX idx_assistant_focus_session ON assistant_focus(session_id, id);

CREATE TABLE assistant_work (
    session_id INTEGER PRIMARY KEY REFERENCES sessions(id),
    about TEXT NOT NULL,
    created_at DATETIME NOT NULL
);

-- rowid = source id * 4 + kind (0 message, 1 unused, 2 memory, 3 focus), so a source row is deleted by rowid.
CREATE VIRTUAL TABLE assistant_search USING fts5(body, scope UNINDEXED);

CREATE TABLE assistant_search_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    message_id INTEGER NOT NULL
);
INSERT INTO assistant_search_state (id, message_id) VALUES (1, 0);

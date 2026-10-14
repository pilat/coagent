-- +goose Up

CREATE UNIQUE INDEX idx_sessions_assistant_root
    ON sessions(json_extract(attributes, '$.manager_id'))
    WHERE agent_type = 'assistant' AND parent_id = 0;

CREATE TABLE assistant_desk (
    session_id INTEGER PRIMARY KEY REFERENCES sessions(id),
    diary TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(diary) AND json_type(diary) = 'object'),
    stickers TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(stickers) AND json_type(stickers) = 'array'),
    focus TEXT NOT NULL DEFAULT '',
    last_turn_at DATETIME,
    updated_at DATETIME NOT NULL
);

CREATE TABLE assistant_tape (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    at DATETIME NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('user', 'say', 'move', 'observe', 'error')),
    text TEXT NOT NULL,
    hidden INTEGER NOT NULL DEFAULT 0 CHECK (hidden IN (0, 1))
);
CREATE INDEX idx_assistant_tape_session ON assistant_tape(session_id, id);

CREATE TABLE assistant_sends (
    input_id INTEGER PRIMARY KEY REFERENCES session_inbox(id),
    assistant_session_id INTEGER NOT NULL REFERENCES sessions(id),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    created_at DATETIME NOT NULL,
    observed_at DATETIME
);
CREATE INDEX idx_assistant_sends_unobserved ON assistant_sends(session_id, observed_at);

CREATE TABLE assistant_turns (
    id INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    input_id INTEGER NOT NULL REFERENCES session_inbox(id),
    step INTEGER NOT NULL,
    desk TEXT NOT NULL,
    response TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'retry', 'failed')),
    created_at DATETIME NOT NULL
);

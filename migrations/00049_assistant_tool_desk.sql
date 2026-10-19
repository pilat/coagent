-- +goose Up

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

-- rowid = source id * 4 + kind (0 message, 1 tape line, 2 memory, 3 focus), so a source row is deleted by rowid.
CREATE VIRTUAL TABLE assistant_search USING fts5(body, scope UNINDEXED);

CREATE TABLE assistant_search_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    message_id INTEGER NOT NULL
);
INSERT INTO assistant_search_state (id, message_id) VALUES (1, 0);

INSERT INTO assistant_work (session_id, about, created_at)
SELECT CAST(substr(n.key, 9) AS INTEGER), n.text,
    CASE WHEN n.written_at > 0 THEN datetime(n.written_at, 'unixepoch') ELSE CURRENT_TIMESTAMP END
FROM assistant_notes n
WHERE n.key GLOB 'session.[0-9]*' AND substr(n.key, 9) NOT GLOB '*[^0-9]*'
    AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = CAST(substr(n.key, 9) AS INTEGER))
ON CONFLICT(session_id) DO NOTHING;

INSERT INTO assistant_memory (session_id, text, until, created_at, updated_at)
SELECT n.session_id,
    CASE
        WHEN n.key GLOB 'project.*' THEN substr(n.key, 9) || ': ' || n.text
        WHEN n.key GLOB 'session.*' THEN 'session ' || substr(n.key, 9) || ': ' || n.text
        ELSE n.text
    END,
    NULL,
    CASE WHEN n.written_at > 0 THEN datetime(n.written_at, 'unixepoch') ELSE CURRENT_TIMESTAMP END,
    CASE WHEN n.written_at > 0 THEN datetime(n.written_at, 'unixepoch') ELSE CURRENT_TIMESTAMP END
FROM assistant_notes n
WHERE n.key GLOB 'user.*' OR n.key GLOB 'rule.*' OR n.key GLOB 'project.*'
    OR (n.key GLOB 'session.*' AND NOT EXISTS (
        SELECT 1 FROM assistant_work w WHERE n.key GLOB 'session.[0-9]*'
            AND substr(n.key, 9) NOT GLOB '*[^0-9]*' AND w.session_id = CAST(substr(n.key, 9) AS INTEGER)
    ))
ORDER BY n.session_id, n.key;

INSERT INTO assistant_focus (session_id, text, sessions, created_at, updated_at)
SELECT n.session_id,
    CASE
        WHEN n.key GLOB 'wait.*' AND NOT (n.key GLOB 'wait.[0-9]*' AND substr(n.key, 6) NOT GLOB '*[^0-9]*')
            THEN substr(n.key, 6) || ': ' || n.text
        ELSE n.text
    END,
    CASE
        WHEN n.key GLOB 'wait.[0-9]*' AND substr(n.key, 6) NOT GLOB '*[^0-9]*'
            THEN json_array(CAST(CAST(substr(n.key, 6) AS INTEGER) AS TEXT))
        ELSE '[]'
    END,
    CASE WHEN n.written_at > 0 THEN datetime(n.written_at, 'unixepoch') ELSE CURRENT_TIMESTAMP END,
    CASE WHEN n.written_at > 0 THEN datetime(n.written_at, 'unixepoch') ELSE CURRENT_TIMESTAMP END
FROM assistant_notes n
WHERE n.key GLOB 'open.*' OR n.key GLOB 'wait.*'
ORDER BY n.session_id, n.key;

INSERT INTO assistant_focus (session_id, text, sessions, created_at, updated_at)
SELECT session_id, focus, '[]', updated_at, updated_at FROM assistant_desk WHERE trim(focus) <> '';

INSERT INTO assistant_search (rowid, body, scope)
SELECT id * 4 + 1, text, session_id FROM assistant_tape WHERE kind IN ('user', 'say');
INSERT INTO assistant_search (rowid, body, scope) SELECT id * 4 + 2, text, session_id FROM assistant_memory;
INSERT INTO assistant_search (rowid, body, scope) SELECT id * 4 + 3, text, session_id FROM assistant_focus;

DROP TABLE assistant_notes;
DROP TABLE assistant_desk;
DROP TABLE assistant_turns;

CREATE TABLE assistant_turns (
    id INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    input_id INTEGER NOT NULL REFERENCES session_inbox(id),
    call INTEGER NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('main', 'final', 'consolidate', 'guard')),
    request TEXT NOT NULL,
    response TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'guard_unresolved', 'failed')),
    created_at DATETIME NOT NULL
);

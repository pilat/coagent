

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


CREATE TABLE IF NOT EXISTS assistant_notes (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    key TEXT NOT NULL,
    text TEXT NOT NULL,
    written_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, key)
);

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id, j.key, j.value, 0
FROM assistant_desk d, json_each(d.diary) j
WHERE j.type = 'text' AND (
    j.key GLOB 'user.*' OR j.key GLOB 'rule.*' OR j.key GLOB 'open.*'
);

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id,
    CASE j.key WHEN 'integration.google-docs' THEN 'open.google-docs' ELSE 'rule.git' END,
    j.value, 0
FROM assistant_desk d, json_each(d.diary) j
WHERE j.type = 'text' AND j.key IN ('integration.google-docs', 'git.policy');

INSERT OR IGNORE INTO assistant_notes (session_id, key, text, written_at)
SELECT d.session_id, 'wait.' || json_extract(j.value, '$.on'), json_extract(j.value, '$.text'), 0
FROM assistant_desk d, json_each(d.stickers) j
WHERE json_type(j.value, '$.on') = 'text'
    AND json_extract(j.value, '$.on') GLOB '[0-9]*'
    AND json_extract(j.value, '$.on') NOT GLOB '*[^0-9]*'
    AND json_type(j.value, '$.text') = 'text';


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

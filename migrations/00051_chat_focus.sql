-- +goose Up

CREATE TABLE IF NOT EXISTS chat_focus (
    manager_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    session_id INTEGER REFERENCES sessions(id),
    PRIMARY KEY (manager_id, chat_id)
);

CREATE TABLE IF NOT EXISTS chat_seen (
    manager_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    last_outbox_id INTEGER NOT NULL CHECK (last_outbox_id >= 0),
    PRIMARY KEY (manager_id, chat_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_session_outbox_session_id
    ON session_outbox(session_id, id);

INSERT INTO chat_seen (manager_id, chat_id, session_id, last_outbox_id)
SELECT b.manager_id, CAST(json_extract(b.attributes, '$.chat_id') AS TEXT),
    o.session_id, MAX(o.id)
FROM session_outbox o
JOIN manager_bindings b ON json_extract(o.attributes, '$.manager_id') = b.manager_id
WHERE o.state = 'delivered'
GROUP BY b.manager_id, CAST(json_extract(b.attributes, '$.chat_id') AS TEXT), o.session_id;

DELETE FROM session_outbox WHERE source_key LIKE 'progress:%';

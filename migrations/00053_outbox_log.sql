-- +goose Up

CREATE TABLE outbox_seq_backup AS SELECT seq FROM sqlite_sequence WHERE name = 'session_outbox';

CREATE TABLE session_outbox_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    type TEXT NOT NULL CHECK (type IN ('message', 'session_opened', 'session_replaced', 'session_closed')),
    content TEXT NOT NULL,
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    source_key TEXT,
    fingerprint TEXT,
    releases_input INTEGER NOT NULL DEFAULT 0 CHECK (releases_input IN (0, 1)),
    created_at DATETIME NOT NULL,
    CHECK ((source_key IS NULL AND fingerprint IS NULL) OR
           (source_key IS NOT NULL AND source_key <> '' AND fingerprint IS NOT NULL AND fingerprint <> ''))
);

INSERT INTO session_outbox_new
    (id, session_id, type, content, attributes, source_key, fingerprint, releases_input, created_at)
SELECT id, session_id,
    CASE WHEN type IN ('message_persistent', 'message_replaceable') THEN 'message' ELSE type END,
    content, attributes, source_key, fingerprint, releases_input, created_at
FROM session_outbox;

DROP TABLE session_outbox;
ALTER TABLE session_outbox_new RENAME TO session_outbox;

CREATE INDEX idx_session_outbox_session_id ON session_outbox(session_id, id);
CREATE UNIQUE INDEX idx_session_outbox_source_key
    ON session_outbox(session_id, source_key) WHERE source_key IS NOT NULL;

DELETE FROM sqlite_sequence WHERE name = 'session_outbox';
INSERT INTO sqlite_sequence(name, seq)
SELECT 'session_outbox', MAX(
    COALESCE((SELECT seq FROM outbox_seq_backup), 0),
    COALESCE((SELECT MAX(id) FROM session_outbox), 0)
);
DROP TABLE outbox_seq_backup;

DROP TABLE manager_bindings;
ALTER TABLE sessions DROP COLUMN manager_reply_pending;

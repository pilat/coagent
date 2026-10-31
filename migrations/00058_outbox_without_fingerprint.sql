-- +goose Up

CREATE TABLE outbox_seq_00058 AS SELECT seq FROM sqlite_sequence WHERE name = 'session_outbox';

DROP INDEX idx_session_outbox_session_id;
DROP INDEX idx_session_outbox_source_key;
ALTER TABLE session_outbox RENAME TO session_outbox_legacy_00058;

CREATE TABLE session_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    type TEXT NOT NULL CHECK (type IN ('message', 'session_opened', 'session_replaced', 'session_closed')),
    content TEXT NOT NULL,
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    source_key TEXT,
    created_at DATETIME NOT NULL,
    CHECK (source_key IS NULL OR source_key <> '')
);

INSERT INTO session_outbox (id, session_id, type, content, attributes, source_key, created_at)
SELECT id, session_id, type, content, attributes, source_key, created_at
FROM session_outbox_legacy_00058;

DROP TABLE session_outbox_legacy_00058;

CREATE INDEX idx_session_outbox_session_id ON session_outbox(session_id, id);
CREATE UNIQUE INDEX idx_session_outbox_source_key
    ON session_outbox(session_id, source_key) WHERE source_key IS NOT NULL;

DELETE FROM sqlite_sequence WHERE name = 'session_outbox';
INSERT INTO sqlite_sequence(name, seq)
SELECT 'session_outbox', MAX(
    COALESCE((SELECT seq FROM outbox_seq_00058), 0),
    COALESCE((SELECT MAX(id) FROM session_outbox), 0)
);
DROP TABLE outbox_seq_00058;

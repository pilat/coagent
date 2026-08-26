-- +goose Up

ALTER TABLE session_inbox
    ADD COLUMN attributes TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(attributes) AND json_type(attributes) = 'object');

CREATE TABLE IF NOT EXISTS session_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    type TEXT NOT NULL CHECK (type IN (
        'message_replaceable', 'message_persistent', 'session_opened',
        'session_replaced', 'session_closed'
    )),
    content TEXT NOT NULL,
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    source_key TEXT,
    fingerprint TEXT,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'delivering', 'retry_wait', 'delivered', 'blocked'
    )),
    attempt_seq INTEGER NOT NULL DEFAULT 0 CHECK (attempt_seq >= 0),
    attempt_id TEXT,
    last_attempt_at DATETIME,
    next_attempt_at DATETIME,
    delivered_at DATETIME,
    blocked_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    CHECK ((source_key IS NULL AND fingerprint IS NULL) OR
           (source_key IS NOT NULL AND source_key <> '' AND fingerprint IS NOT NULL AND fingerprint <> '')),
    CHECK (
        (state = 'pending' AND attempt_seq = 0 AND attempt_id IS NULL
            AND last_attempt_at IS NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'delivering' AND attempt_seq > 0 AND attempt_id IS NOT NULL AND attempt_id <> ''
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'retry_wait' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NOT NULL AND delivered_at IS NULL
            AND blocked_at IS NULL AND last_error <> '')
        OR (state = 'delivered' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NOT NULL
            AND blocked_at IS NULL AND last_error = '')
        OR (state = 'blocked' AND attempt_seq > 0 AND attempt_id IS NULL
            AND last_attempt_at IS NOT NULL AND next_attempt_at IS NULL AND delivered_at IS NULL
            AND blocked_at IS NOT NULL AND last_error <> '')
    )
);

CREATE INDEX IF NOT EXISTS idx_session_outbox_manager_head
    ON session_outbox(json_extract(attributes, '$.manager_id'), id)
    WHERE state <> 'delivered';

CREATE INDEX IF NOT EXISTS idx_session_outbox_delivered_message
    ON session_outbox(session_id, id DESC)
    WHERE state = 'delivered' AND type IN ('message_replaceable', 'message_persistent');

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_outbox_source_key
    ON session_outbox(session_id, source_key)
    WHERE source_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS manager_bindings (
    manager_id TEXT PRIMARY KEY CHECK (manager_id <> ''),
    driver TEXT NOT NULL CHECK (driver <> ''),
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object' AND attributes <> '{}'),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

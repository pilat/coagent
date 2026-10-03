-- +goose Up

-- Deleted inbox IDs remain reserved across the table rebuild.
CREATE TEMP TABLE session_inbox_sequence_00045 (seq INTEGER NOT NULL);
INSERT INTO session_inbox_sequence_00045
SELECT seq FROM sqlite_sequence WHERE name = 'session_inbox';

DROP INDEX idx_session_tool_activations_pending;
ALTER TABLE session_tool_activations RENAME TO session_tool_activations_legacy_00045;
DROP INDEX idx_subagent_links_undelivered;
DROP INDEX idx_subagent_links_child;
ALTER TABLE subagent_links RENAME TO subagent_links_legacy_00045;
DROP INDEX idx_session_inbox_pending_fifo;
DROP INDEX idx_session_inbox_accepted_session;
ALTER TABLE session_inbox RENAME TO session_inbox_legacy_00045;

CREATE TABLE session_inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    source TEXT NOT NULL CHECK (source IN ('user', 'agent', 'process', 'subagent', 'schedule', 'call_result')),
    raw_content TEXT NOT NULL CHECK (raw_content <> ''),
    received_at DATETIME NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'accepted', 'handled', 'rejected', 'cancelled')),
    resolved_at DATETIME,
    resolution_reason TEXT,
    accepted_message_id INTEGER REFERENCES messages(id),
    attributes TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    delivery_key TEXT,
    CHECK (
        (state = 'pending' AND resolved_at IS NULL AND resolution_reason IS NULL AND accepted_message_id IS NULL)
        OR (state = 'accepted' AND resolved_at IS NOT NULL AND resolution_reason IS NULL AND accepted_message_id IS NOT NULL)
        OR (state IN ('handled', 'rejected', 'cancelled') AND resolved_at IS NOT NULL
            AND resolution_reason IS NOT NULL AND resolution_reason <> '' AND accepted_message_id IS NULL)
    )
);

INSERT INTO session_inbox
    (id, session_id, source, raw_content, received_at, state, resolved_at,
     resolution_reason, accepted_message_id, attributes)
SELECT id, session_id, source, raw_content, received_at, state, resolved_at,
       resolution_reason, accepted_message_id, attributes
FROM session_inbox_legacy_00045;

CREATE TABLE session_tool_activations (
    input_id INTEGER PRIMARY KEY REFERENCES session_inbox(id),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    tool_id TEXT NOT NULL CHECK (tool_id <> ''),
    command TEXT NOT NULL CHECK (command <> '' AND substr(command, 1, 1) = '/'),
    state TEXT NOT NULL CHECK (state IN ('pending', 'consumed', 'expired')),
    tool_call_id TEXT,
    created_at DATETIME NOT NULL,
    resolved_at DATETIME,
    CHECK (
        (state = 'pending' AND tool_call_id IS NULL AND resolved_at IS NULL)
        OR (state = 'consumed' AND tool_call_id IS NOT NULL AND tool_call_id <> '' AND resolved_at IS NOT NULL)
        OR (state = 'expired' AND tool_call_id IS NULL AND resolved_at IS NOT NULL)
    )
);

INSERT INTO session_tool_activations
SELECT * FROM session_tool_activations_legacy_00045;

CREATE TABLE subagent_links (
    parent_id INTEGER NOT NULL,
    child_id INTEGER NOT NULL,
    task_call_id TEXT NOT NULL,
    blocking INTEGER NOT NULL DEFAULT 0,
    depth INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'spawned',
    delivered_at INTEGER,
    delivered_msg_id INTEGER,
    created_at INTEGER NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    activation_seq INTEGER NOT NULL DEFAULT 1,
    delivered_input_id INTEGER REFERENCES session_inbox(id),
    PRIMARY KEY (parent_id, child_id)
);

INSERT INTO subagent_links
SELECT * FROM subagent_links_legacy_00045;

INSERT INTO sqlite_sequence (name, seq)
SELECT 'session_inbox', seq FROM session_inbox_sequence_00045
WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'session_inbox');
UPDATE sqlite_sequence
SET seq = MAX(seq, COALESCE((SELECT seq FROM session_inbox_sequence_00045), seq))
WHERE name = 'session_inbox';
DROP TABLE session_inbox_sequence_00045;

DROP TABLE session_tool_activations_legacy_00045;
DROP TABLE subagent_links_legacy_00045;
DROP TABLE session_inbox_legacy_00045;

CREATE UNIQUE INDEX idx_session_tool_activations_pending
    ON session_tool_activations(session_id) WHERE state = 'pending';
CREATE INDEX idx_subagent_links_undelivered
    ON subagent_links(parent_id) WHERE delivered_at IS NULL;
CREATE INDEX idx_subagent_links_child ON subagent_links(child_id);
CREATE INDEX idx_session_inbox_pending_fifo
    ON session_inbox(session_id, id) WHERE state = 'pending';
CREATE INDEX idx_session_inbox_accepted_session
    ON session_inbox(session_id, id) WHERE state = 'accepted' AND accepted_message_id IS NOT NULL;
CREATE UNIQUE INDEX idx_session_inbox_delivery
    ON session_inbox(session_id, delivery_key) WHERE delivery_key IS NOT NULL;

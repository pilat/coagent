-- +goose Up

CREATE TABLE IF NOT EXISTS session_inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    source TEXT NOT NULL CHECK (source IN ('user', 'agent')),
    raw_content TEXT NOT NULL CHECK (raw_content <> ''),
    received_at DATETIME NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'accepted', 'handled', 'rejected', 'cancelled')),
    resolved_at DATETIME,
    resolution_reason TEXT,
    accepted_message_id INTEGER REFERENCES messages(id),
    CHECK (
        (state = 'pending'
            AND resolved_at IS NULL
            AND resolution_reason IS NULL
            AND accepted_message_id IS NULL)
        OR (state = 'accepted'
            AND resolved_at IS NOT NULL
            AND resolution_reason IS NULL
            AND accepted_message_id IS NOT NULL)
        OR (state IN ('handled', 'rejected', 'cancelled')
            AND resolved_at IS NOT NULL
            AND resolution_reason IS NOT NULL
            AND resolution_reason <> ''
            AND accepted_message_id IS NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_session_inbox_pending_fifo
    ON session_inbox(session_id, id) WHERE state = 'pending';

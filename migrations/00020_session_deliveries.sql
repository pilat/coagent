-- +goose Up

-- Durable idempotency keys for external events that mutate a transcript before
-- their producer can acknowledge its own ledger. The claim and transcript
-- mutation commit in one SQLite transaction, so a crash leaves either both or
-- neither. A fingerprint makes accidental key reuse fail closed.
CREATE TABLE session_deliveries (
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    delivery_id TEXT NOT NULL CHECK (delivery_id <> ''),
    kind TEXT NOT NULL CHECK (kind IN ('tool_notification', 'context_reset')),
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    delivered_at DATETIME NOT NULL,
    PRIMARY KEY (session_id, delivery_id)
);


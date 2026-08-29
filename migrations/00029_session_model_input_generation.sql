-- +goose Up

ALTER TABLE sessions ADD COLUMN model_input_generation INTEGER NOT NULL DEFAULT 0
    CHECK (model_input_generation >= 0);

ALTER TABLE sessions ADD COLUMN model_input_boundary INTEGER;

-- Legacy sessions with transcript history are fail-safe anchored at their latest
-- row: pre-upgrade narration is suppressed until a new generated turn stores it.
UPDATE sessions SET
    model_input_generation = 1,
    model_input_boundary = (SELECT MAX(m.id) FROM messages m WHERE m.session_id = sessions.id)
WHERE EXISTS (
    SELECT 1 FROM messages m WHERE m.session_id = sessions.id
);

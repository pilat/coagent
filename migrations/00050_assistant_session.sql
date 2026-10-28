-- +goose Up

CREATE TABLE IF NOT EXISTS assistant_sends (
    input_id INTEGER PRIMARY KEY REFERENCES session_inbox(id),
    assistant_session_id INTEGER NOT NULL REFERENCES sessions(id),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    created_at DATETIME NOT NULL,
    observed_at DATETIME
);

UPDATE session_inbox
SET attributes = json_set(attributes, '$.observer_session_id', (
    SELECT assistant_session_id FROM assistant_sends WHERE input_id = session_inbox.id
))
WHERE id IN (SELECT input_id FROM assistant_sends WHERE observed_at IS NULL);

DROP TABLE assistant_sends;
DROP TABLE IF EXISTS assistant_tape;
DROP TABLE IF EXISTS assistant_turns;
DROP TABLE IF EXISTS assistant_notes;
DROP TABLE IF EXISTS assistant_desk;
DELETE FROM assistant_search WHERE rowid % 4 = 1;

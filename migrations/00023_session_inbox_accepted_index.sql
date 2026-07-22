-- +goose Up

CREATE INDEX IF NOT EXISTS idx_session_inbox_accepted_session
    ON session_inbox(session_id, id)
    WHERE state = 'accepted' AND accepted_message_id IS NOT NULL;

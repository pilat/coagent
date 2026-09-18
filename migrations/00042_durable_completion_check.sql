-- +goose Up

ALTER TABLE sessions ADD COLUMN completion_check_candidate_id INTEGER
    REFERENCES messages(id);
ALTER TABLE sessions ADD COLUMN manager_reply_pending BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sessions ADD COLUMN empty_stop_streak INTEGER NOT NULL DEFAULT 0
    CHECK (empty_stop_streak >= 0);

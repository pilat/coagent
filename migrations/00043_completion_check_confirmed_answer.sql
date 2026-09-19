-- +goose Up

ALTER TABLE sessions ADD COLUMN completion_check_confirmed_answer_id INTEGER
    REFERENCES messages(id);

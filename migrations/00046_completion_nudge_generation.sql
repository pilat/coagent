-- +goose Up

ALTER TABLE sessions ADD COLUMN completion_nudge_generation INTEGER;

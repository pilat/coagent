-- +goose Up
ALTER TABLE messages ADD COLUMN position INTEGER;
UPDATE messages SET position = id;

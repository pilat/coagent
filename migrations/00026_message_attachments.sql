-- +goose Up
ALTER TABLE messages ADD COLUMN attachments TEXT;

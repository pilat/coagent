-- +goose Up
ALTER TABLE messages ADD COLUMN reasoning_raw TEXT;

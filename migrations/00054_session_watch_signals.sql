-- +goose Up

ALTER TABLE session_watches ADD COLUMN subscribed INTEGER NOT NULL DEFAULT 0 CHECK (subscribed IN (0, 1));
ALTER TABLE session_watches ADD COLUMN armed INTEGER NOT NULL DEFAULT 0 CHECK (armed IN (0, 1));
ALTER TABLE session_watches ADD COLUMN signalled INTEGER NOT NULL DEFAULT 0 CHECK (signalled IN (0, 1));

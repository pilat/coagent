-- +goose Up
-- Explicit subagent result + completed/incomplete outcome stored on the link, so
-- the completion delivered to the parent is read from a durable column instead of
-- re-scraping the child's transcript.
--
-- SQLite has no ADD COLUMN IF NOT EXISTS; goose version tracking runs this exactly
-- once, so the bare ADD COLUMN is safe here (deviates from the IF-NOT-EXISTS habit
-- by necessity).

ALTER TABLE subagent_links ADD COLUMN result TEXT NOT NULL DEFAULT '';
ALTER TABLE subagent_links ADD COLUMN outcome TEXT NOT NULL DEFAULT '';

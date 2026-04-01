-- +goose Up
-- Append-only context log: cleared_at marks a tool result whose body is replaced
-- by a uniform placeholder in the rendered view; the stored content stays intact.
ALTER TABLE messages ADD COLUMN cleared_at DATETIME;

-- Drop the extraction/embedding memory subsystem. Children before parents; the
-- FTS5 virtual table may not exist on platforms without FTS5 — IF EXISTS covers it.
DROP TABLE IF EXISTS extraction_chunks_fts;
DROP TABLE IF EXISTS extraction_chunks;
DROP TABLE IF EXISTS extractions;
DROP TABLE IF EXISTS memory_meta;

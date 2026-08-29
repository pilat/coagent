-- +goose Up
-- Drop the trim-before-summary compaction state superseded by ADR-0035:
-- cleared_at placeholders and the duplicate sessions.compaction_brief copy.
-- Stored tool bodies were never deleted, so dropping cleared_at restores any
-- formerly cleared active result beside its call; compacted rows stay hidden.
ALTER TABLE messages DROP COLUMN cleared_at;
ALTER TABLE sessions DROP COLUMN compaction_brief;

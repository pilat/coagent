-- +goose Up
-- ADR-0039: agent work has no host-imposed wall-clock lifetime, so the dropped
-- timeout_sec column describes removed behavior and carries no data worth keeping.
ALTER TABLE subagent_links DROP COLUMN timeout_sec;

-- +goose Up

-- A child id identifies a reusable subagent session, not one particular run of
-- that session. Completion signals carry this internal sequence so a delayed
-- duplicate from an older activation cannot be delivered after rearm.
ALTER TABLE subagent_links
    ADD COLUMN activation_seq INTEGER NOT NULL DEFAULT 1;


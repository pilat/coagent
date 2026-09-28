-- +goose Up

ALTER TABLE sessions DROP COLUMN master_enabled;
ALTER TABLE subagent_links DROP COLUMN delivered_msg_id;

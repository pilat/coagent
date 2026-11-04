-- +goose Up

ALTER TABLE sessions DROP COLUMN model_input_boundary;

DROP INDEX idx_subagent_links_undelivered;
DROP INDEX idx_subagent_links_child;
ALTER TABLE subagent_links RENAME TO subagent_links_legacy;

CREATE TABLE subagent_links (
    parent_id INTEGER NOT NULL,
    child_id INTEGER NOT NULL,
    task_call_id TEXT NOT NULL,
    blocking INTEGER NOT NULL DEFAULT 0,
    depth INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'spawned',
    delivered_at INTEGER,
    created_at INTEGER NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    activation_seq INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (parent_id, child_id)
);

INSERT INTO subagent_links
    (parent_id, child_id, task_call_id, blocking, depth, state, delivered_at,
     created_at, result, outcome, activation_seq)
SELECT parent_id, child_id, task_call_id, blocking, depth, state, delivered_at,
       created_at, result, outcome, activation_seq
FROM subagent_links_legacy;

DROP TABLE subagent_links_legacy;

CREATE INDEX idx_subagent_links_undelivered
    ON subagent_links(parent_id) WHERE delivered_at IS NULL;
CREATE INDEX idx_subagent_links_child ON subagent_links(child_id);

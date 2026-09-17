-- +goose Up
-- One live management root per manager owner: repeated or concurrent startup
-- ensures return the same row instead of inserting another. /clear moves the
-- old root to terminating before the successor insert, so the predicate keeps
-- it out of the uniqueness set.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_management_root
    ON sessions(project_id, json_extract(attributes, '$.manager_id'))
    WHERE parent_id = 0
      AND killed_at IS NULL
      AND status NOT IN ('terminating', 'killed')
      AND json_extract(attributes, '$.management_surface') IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_management_root;

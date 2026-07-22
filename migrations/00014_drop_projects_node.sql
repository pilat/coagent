-- +goose Up
-- Children before parents: the session set is derived from projects.node, so
-- projects must go last. subagent_links has no FK, so its order is on us.

DELETE FROM messages WHERE session_id IN (
    SELECT id FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE node != 'local')
);
DELETE FROM schedules WHERE session_id IN (
    SELECT id FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE node != 'local')
);
DELETE FROM subagent_links WHERE parent_id IN (
    SELECT id FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE node != 'local')
) OR child_id IN (
    SELECT id FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE node != 'local')
);
DELETE FROM memories WHERE project_id IN (SELECT id FROM projects WHERE node != 'local');
DELETE FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE node != 'local');
DELETE FROM projects WHERE node != 'local';

-- Model IDs were stored with the node prefix the picker attached; without nodes
-- the bare ID is the only form the resolver understands.
UPDATE sessions SET model = substr(model, 7) WHERE model LIKE 'local:%';

DROP INDEX IF EXISTS idx_projects_workdir_node;
ALTER TABLE projects DROP COLUMN node;
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_workdir ON projects(work_dir);

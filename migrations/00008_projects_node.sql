-- +goose NO TRANSACTION
-- +goose Up
-- Recreate projects table with node column and composite unique key.
-- SQLite doesn't support DROP CONSTRAINT, so we recreate the table.
-- Must disable FKs because sessions/memories/extractions reference projects(id).

PRAGMA foreign_keys = OFF;

CREATE TABLE IF NOT EXISTS projects_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    work_dir TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    node TEXT NOT NULL DEFAULT 'local'
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_workdir_node ON projects_new(work_dir, node);

-- Copy existing data (all existing projects are local).
INSERT OR IGNORE INTO projects_new (id, work_dir, name, node)
    SELECT id, work_dir, name, 'local' FROM projects;

DROP TABLE IF EXISTS projects;
ALTER TABLE projects_new RENAME TO projects;

PRAGMA foreign_keys = ON;

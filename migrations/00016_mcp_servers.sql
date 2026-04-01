-- +goose Up
CREATE TABLE IF NOT EXISTS mcp_servers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER REFERENCES projects(id),
    name TEXT NOT NULL,
    command TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '[]',
    env TEXT NOT NULL DEFAULT '{}',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- Partial indexes: plain UNIQUE treats NULLs as distinct, so global rows would
-- not collide on name.
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_servers_project_name ON mcp_servers(project_id, name) WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_servers_global_name ON mcp_servers(name) WHERE project_id IS NULL;

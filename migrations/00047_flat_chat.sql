-- +goose Up

PRAGMA defer_foreign_keys = ON;

CREATE TABLE IF NOT EXISTS chat_focus (
    manager_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    session_id INTEGER REFERENCES sessions(id), last_sent_id INTEGER,
    PRIMARY KEY (manager_id, chat_id)
);

DELETE FROM session_outbox WHERE source_key LIKE 'progress:%';

CREATE TEMP TABLE doomed_sessions AS
WITH RECURSIVE doomed(id) AS (
    SELECT s.id FROM sessions s
    WHERE s.project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden)
        OR (s.parent_id = 0 AND json_type(s.attributes, '$.management_surface') IS NOT NULL)
    UNION
    SELECT child.id FROM sessions child JOIN doomed ON child.parent_id = doomed.id
)
SELECT id FROM doomed;

INSERT INTO doomed_sessions
SELECT id FROM sessions WHERE root_id IN (SELECT id FROM doomed_sessions)
    AND id NOT IN (SELECT id FROM doomed_sessions);

DELETE FROM session_tool_activations WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM messages WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM schedules WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM session_inbox WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM session_outbox WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM session_budgets WHERE root_session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM session_file_reads WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM session_deliveries WHERE session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM background_processes
WHERE session_id IN (SELECT id FROM doomed_sessions) OR root_session_id IN (SELECT id FROM doomed_sessions);
DELETE FROM subagent_links
WHERE parent_id IN (SELECT id FROM doomed_sessions) OR child_id IN (SELECT id FROM doomed_sessions);
DELETE FROM memories WHERE project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden);
DELETE FROM mcp_servers WHERE project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden);
DELETE FROM sessions WHERE id IN (SELECT id FROM doomed_sessions);
DELETE FROM projects WHERE name = 'sys_coagent' AND hidden;

DROP TABLE doomed_sessions;

UPDATE sessions
SET attributes = json_remove(attributes, '$.management_surface', '$.telegram_topic_id')
WHERE json_type(attributes, '$.management_surface') IS NOT NULL
    OR json_type(attributes, '$.telegram_topic_id') IS NOT NULL;

DROP INDEX IF EXISTS idx_sessions_management_root;

CREATE TEMP TABLE outbox_seq_backup AS SELECT seq FROM sqlite_sequence WHERE name = 'session_outbox';

ALTER TABLE session_outbox RENAME TO session_outbox_legacy;

CREATE TABLE session_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    type TEXT NOT NULL CHECK (type IN ('message', 'session_opened', 'session_replaced', 'session_closed')),
    content TEXT NOT NULL,
    attributes TEXT NOT NULL CHECK (json_valid(attributes) AND json_type(attributes) = 'object'),
    source_key TEXT,
    created_at DATETIME NOT NULL,
    CHECK (source_key IS NULL OR source_key <> '')
);

INSERT INTO session_outbox (id, session_id, type, content, attributes, source_key, created_at)
SELECT id, session_id,
    CASE WHEN type IN ('message_persistent', 'message_replaceable') THEN 'message' ELSE type END,
    content, attributes, source_key, created_at
FROM session_outbox_legacy;

DROP TABLE session_outbox_legacy;

CREATE INDEX idx_session_outbox_session_id ON session_outbox(session_id, id);
CREATE UNIQUE INDEX idx_session_outbox_source_key
    ON session_outbox(session_id, source_key) WHERE source_key IS NOT NULL;

DELETE FROM sqlite_sequence WHERE name = 'session_outbox';
INSERT INTO sqlite_sequence(name, seq)
SELECT 'session_outbox', MAX(
    COALESCE((SELECT seq FROM outbox_seq_backup), 0),
    COALESCE((SELECT MAX(id) FROM session_outbox), 0)
);
DROP TABLE outbox_seq_backup;

DROP TABLE manager_bindings;
ALTER TABLE sessions DROP COLUMN manager_reply_pending;

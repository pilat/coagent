-- +goose Up

PRAGMA defer_foreign_keys = ON;

CREATE TEMP TABLE doomed_00059 AS
WITH RECURSIVE doomed(id) AS (
    SELECT s.id FROM sessions s
    WHERE s.project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden)
        OR (s.agent_type = 'assistant' AND s.parent_id = 0)
    UNION
    SELECT child.id FROM sessions child JOIN doomed ON child.parent_id = doomed.id
)
SELECT id FROM doomed;

INSERT INTO doomed_00059
SELECT id FROM sessions WHERE root_id IN (SELECT id FROM doomed_00059)
    AND id NOT IN (SELECT id FROM doomed_00059);

ALTER TABLE chat_focus ADD COLUMN last_sent_id INTEGER;

UPDATE chat_focus
SET last_sent_id = (
    SELECT seen.last_outbox_id FROM chat_seen seen
    WHERE seen.manager_id = chat_focus.manager_id AND seen.chat_id = chat_focus.chat_id
        AND seen.session_id = chat_focus.session_id
);

UPDATE chat_focus SET session_id = NULL, last_sent_id = NULL
WHERE session_id IN (SELECT id FROM doomed_00059);

DROP TABLE chat_seen;

DROP TABLE IF EXISTS assistant_memory;
DROP TABLE IF EXISTS assistant_focus;
DROP TABLE IF EXISTS assistant_search;
DROP TABLE IF EXISTS assistant_search_state;
DROP INDEX IF EXISTS idx_sessions_assistant_root;

DELETE FROM session_tool_activations WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM messages WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM schedules WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM session_inbox WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM session_outbox WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM session_budgets WHERE root_session_id IN (SELECT id FROM doomed_00059);
DELETE FROM session_file_reads WHERE session_id IN (SELECT id FROM doomed_00059);
DELETE FROM background_processes
WHERE session_id IN (SELECT id FROM doomed_00059) OR root_session_id IN (SELECT id FROM doomed_00059);
DELETE FROM subagent_links
WHERE parent_id IN (SELECT id FROM doomed_00059) OR child_id IN (SELECT id FROM doomed_00059);
DELETE FROM memories WHERE project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden);
DELETE FROM mcp_servers WHERE project_id IN (SELECT id FROM projects WHERE name = 'sys_coagent' AND hidden);
DELETE FROM sessions WHERE id IN (SELECT id FROM doomed_00059);
DELETE FROM projects WHERE name = 'sys_coagent' AND hidden;

UPDATE session_inbox
SET attributes = json_remove(attributes, '$.observer_session_id', '$.from', '$.forwarded_by', '$.forwarded_input_id', '$.observed_session_id', '$.observed_kind', '$.observed_message_id', '$.schedule_id', '$.accepted_run')
WHERE json_type(attributes, '$.observer_session_id') IS NOT NULL
    OR json_type(attributes, '$.from') IS NOT NULL
    OR json_type(attributes, '$.forwarded_by') IS NOT NULL
    OR json_type(attributes, '$.forwarded_input_id') IS NOT NULL
    OR json_type(attributes, '$.observed_session_id') IS NOT NULL
    OR json_type(attributes, '$.observed_kind') IS NOT NULL
    OR json_type(attributes, '$.observed_message_id') IS NOT NULL
    OR json_type(attributes, '$.schedule_id') IS NOT NULL
    OR json_type(attributes, '$.accepted_run') IS NOT NULL;

UPDATE session_outbox
SET attributes = json_remove(attributes, '$.relayed_session_id', '$.relayed_output_id', '$.forwarded_session_id')
WHERE json_type(attributes, '$.relayed_session_id') IS NOT NULL
    OR json_type(attributes, '$.relayed_output_id') IS NOT NULL
    OR json_type(attributes, '$.forwarded_session_id') IS NOT NULL;

UPDATE schedules
SET metadata = json_remove(metadata, '$.report_to')
WHERE json_type(metadata, '$.report_to') IS NOT NULL;

UPDATE sessions
SET attributes = json_remove(attributes, '$.current_run')
WHERE json_type(attributes, '$.current_run') IS NOT NULL;

DROP TABLE doomed_00059;

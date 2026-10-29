-- +goose Up

CREATE TEMP TABLE retired_management_00052 AS
WITH RECURSIVE retired(id, manager_id) AS (
    SELECT id, json_extract(attributes, '$.manager_id')
    FROM sessions
    WHERE parent_id = 0 AND json_type(attributes, '$.management_surface') IS NOT NULL
    UNION
    SELECT child.id, retired.manager_id
    FROM sessions child JOIN retired ON child.parent_id = retired.id
)
SELECT id, manager_id FROM retired;

UPDATE sessions
SET status = 'killed', killed_at = COALESCE(killed_at, CURRENT_TIMESTAMP)
WHERE id IN (SELECT id FROM retired_management_00052);

DELETE FROM schedules
WHERE session_id IN (SELECT id FROM retired_management_00052)
    AND one_shot_at IS NOT NULL
    AND COALESCE(json_extract(metadata, '$.tool_call_id'), '') <> '';

UPDATE schedules AS scheduled
SET session_id = (
    SELECT assistant.id
    FROM sessions assistant
    JOIN retired_management_00052 retired ON retired.id = scheduled.session_id
    WHERE assistant.agent_type = 'assistant' AND assistant.parent_id = 0
        AND json_extract(assistant.attributes, '$.manager_id') = retired.manager_id
)
WHERE scheduled.session_id IN (SELECT id FROM retired_management_00052)
    AND EXISTS (
        SELECT 1
        FROM sessions assistant
        JOIN retired_management_00052 retired ON retired.id = scheduled.session_id
        WHERE assistant.agent_type = 'assistant' AND assistant.parent_id = 0
            AND json_extract(assistant.attributes, '$.manager_id') = retired.manager_id
    );

DELETE FROM schedules WHERE session_id IN (SELECT id FROM retired_management_00052);
DELETE FROM session_budgets WHERE root_session_id IN (SELECT id FROM retired_management_00052);

UPDATE sessions
SET attributes = json_remove(attributes, '$.management_surface', '$.telegram_topic_id')
WHERE json_type(attributes, '$.management_surface') IS NOT NULL
    OR json_type(attributes, '$.telegram_topic_id') IS NOT NULL;

DROP INDEX IF EXISTS idx_sessions_management_root;
DROP TABLE retired_management_00052;

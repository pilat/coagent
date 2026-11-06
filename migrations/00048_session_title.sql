-- +goose Up

WITH inputs AS (
    SELECT id, session_id, trim(raw_content, ' ' || char(9, 10, 13)) AS content
    FROM session_inbox WHERE source = 'user'
), lines AS (
    SELECT id, session_id,
        replace(content, char(13), char(10)) AS content
    FROM inputs WHERE content <> '' AND substr(content, 1, 1) <> '/'
), titles AS (
    SELECT id, session_id,
        trim(substr(content, 1, instr(content || char(10), char(10)) - 1),
            ' ' || char(9, 10, 13)) AS title
    FROM lines
), ranked AS (
    SELECT session_id, title, row_number() OVER (PARTITION BY session_id ORDER BY id) AS position
    FROM titles WHERE title <> ''
), first_titles AS (
    SELECT session_id,
        CASE WHEN length(title) > 80 THEN substr(title, 1, 79) || '…' ELSE title END AS title
    FROM ranked WHERE position = 1
)
UPDATE sessions
SET attributes = json_set(COALESCE(attributes, '{}'), '$.title', (
    SELECT title FROM first_titles WHERE session_id = sessions.id
))
WHERE parent_id = 0 AND COALESCE(json_extract(attributes, '$.manager_id'), '') <> ''
    AND json_extract(attributes, '$.title') IS NULL
    AND id IN (SELECT session_id FROM first_titles);

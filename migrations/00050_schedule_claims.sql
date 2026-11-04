-- +goose Up

CREATE TEMP TABLE schedule_claims AS
SELECT session_id, delivery_key AS delivery_id
FROM session_inbox
WHERE delivery_key GLOB 'schedule:*' OR delivery_key GLOB 'sleep:*'
UNION
SELECT session_id, delivery_id
FROM session_deliveries
WHERE delivery_id GLOB 'schedule:*' OR delivery_id GLOB 'sleep:*';

CREATE INDEX idx_schedule_claims ON schedule_claims(session_id, delivery_id);

DELETE FROM schedules
WHERE one_shot_at IS NOT NULL AND EXISTS (
    SELECT 1 FROM schedule_claims c
    WHERE c.session_id = schedules.session_id
      AND c.delivery_id IN ('schedule:one-shot:' || schedules.id, 'sleep:' || schedules.id)
);

-- Match Go's UTC encoding so the runtime's textual minute guard stays closed.
WITH occurrences AS (
    SELECT s.id, MAX(datetime(
        substr(c.delivery_id, -14, 4) || '-' || substr(c.delivery_id, -10, 2) || '-' ||
        substr(c.delivery_id, -8, 2) || ' ' || substr(c.delivery_id, -5, 2) || ':' ||
        substr(c.delivery_id, -3, 2) || ':00'
    ) || ' +0000 UTC') AS last_fired_at
    FROM schedules s
    JOIN schedule_claims c ON c.session_id = s.session_id
      AND c.delivery_id GLOB 'schedule:cron:' || s.id || ':[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9]Z'
    WHERE s.cron_expr IS NOT NULL
    GROUP BY s.id
)
UPDATE schedules SET last_fired_at = occurrences.last_fired_at
FROM occurrences
WHERE schedules.id = occurrences.id AND occurrences.last_fired_at IS NOT NULL
  AND (schedules.last_fired_at IS NULL OR
       datetime(substr(schedules.last_fired_at, 1, 19)) < datetime(substr(occurrences.last_fired_at, 1, 19)));

DROP TABLE schedule_claims;
DROP TABLE IF EXISTS session_deliveries;

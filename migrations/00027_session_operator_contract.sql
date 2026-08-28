-- +goose Up

ALTER TABLE session_outbox
    ADD COLUMN releases_input INTEGER NOT NULL DEFAULT 0
    CHECK (releases_input IN (0, 1));

ALTER TABLE sessions ADD COLUMN episode_started_at DATETIME;

WITH model_input AS (
    SELECT session_id, state, received_at
    FROM (
        SELECT session_id, source, state, received_at,
            trim(raw_content, char(
                9, 10, 11, 12, 13, 32, 133, 160, 5760,
                8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
                8232, 8233, 8239, 8287, 12288
            )) AS content
        FROM session_inbox
    )
    WHERE source = 'user'
        AND content NOT IN ('/status', '/help', '/schedules', '/stop', '/clear', '/kill', '/compact')
        AND content NOT GLOB '/compact *'
)
UPDATE sessions AS root SET episode_started_at = CASE
    WHEN root.status IN ('completed', 'error')
        AND NOT EXISTS (
            SELECT 1 FROM sessions child
            WHERE child.root_id = root.id AND child.status IN ('active', 'suspended')
        )
    THEN (
        SELECT MAX(pending.received_at) FROM model_input pending
        WHERE pending.session_id = root.id AND pending.state = 'pending'
    )
    ELSE root.updated_at
END
WHERE root.parent_id = 0
    AND (
        (
            EXISTS (
                SELECT 1 FROM model_input input
                WHERE input.session_id = root.id
                    AND input.state IN ('pending', 'accepted')
            )
            AND (
                root.status IN ('active', 'suspended')
                OR EXISTS (
                    SELECT 1 FROM sessions child
                    WHERE child.root_id = root.id AND child.status IN ('active', 'suspended')
                )
                OR EXISTS (
                    SELECT 1 FROM model_input pending
                    WHERE pending.session_id = root.id
                        AND pending.state = 'pending'
                )
            )
        )
        OR (
            root.status IN ('active', 'suspended')
            AND EXISTS (
                SELECT 1 FROM session_outbox output
                WHERE output.session_id = root.id
                    AND json_extract(output.attributes, '$.source') = 'scheduler'
            )
        )
    );

CREATE TABLE IF NOT EXISTS session_tool_activations (
    input_id INTEGER PRIMARY KEY REFERENCES session_inbox(id),
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    tool_id TEXT NOT NULL CHECK (tool_id <> ''),
    command TEXT NOT NULL CHECK (command <> '' AND substr(command, 1, 1) = '/'),
    state TEXT NOT NULL CHECK (state IN ('pending', 'consumed', 'expired')),
    tool_call_id TEXT,
    created_at DATETIME NOT NULL,
    resolved_at DATETIME,
    CHECK (
        (state = 'pending' AND tool_call_id IS NULL AND resolved_at IS NULL)
        OR (state = 'consumed' AND tool_call_id IS NOT NULL AND tool_call_id <> '' AND resolved_at IS NOT NULL)
        OR (state = 'expired' AND tool_call_id IS NULL AND resolved_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_tool_activations_pending
    ON session_tool_activations(session_id)
    WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS session_budgets (
    root_session_id INTEGER PRIMARY KEY REFERENCES sessions(id),
    state TEXT NOT NULL CHECK (state IN ('armed', 'fired', 'released')),
    generation INTEGER NOT NULL CHECK (generation > 0),
    armed_at DATETIME NOT NULL,
    baseline_cost_usd REAL NOT NULL
        CHECK (baseline_cost_usd >= 0 AND baseline_cost_usd <= 1000000000000),
    cost_limit_usd REAL
        CHECK (cost_limit_usd > 0 AND cost_limit_usd <= 1000000),
    duration_seconds INTEGER
        CHECK (duration_seconds BETWEEN 60 AND 31536000),
    fired_at DATETIME,
    released_at DATETIME,
    fired_reason TEXT NOT NULL DEFAULT '' CHECK (fired_reason IN ('', 'cost', 'duration')),
    released_reason TEXT NOT NULL DEFAULT '' CHECK (released_reason IN (
        '', 'cleared', 'resumed', 'completed', 'error', 'stopped', 'killed', 'replaced'
    )),
    observed_cost_usd REAL
        CHECK (observed_cost_usd >= 0 AND observed_cost_usd <= 1000000000000),
    park_phase TEXT NOT NULL DEFAULT '' CHECK (park_phase IN ('', 'requested', 'draining', 'parked')),
    park_owner TEXT NOT NULL DEFAULT '',
    CHECK (
        (state = 'armed'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NULL AND released_at IS NULL
            AND fired_reason = '' AND released_reason = '' AND observed_cost_usd IS NULL
            AND park_phase = '' AND park_owner = '')
        OR (state = 'fired'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NOT NULL AND released_at IS NULL
            AND fired_reason <> '' AND released_reason = '' AND observed_cost_usd IS NOT NULL
            AND park_phase <> ''
            AND ((park_phase IN ('requested', 'draining') AND park_owner <> '')
                OR (park_phase = 'parked' AND park_owner = '')))
        OR (state = 'released'
            AND released_at IS NOT NULL AND released_reason <> ''
            AND park_owner = '')
    )
);

CREATE INDEX IF NOT EXISTS idx_messages_session_history
    ON messages(session_id, id);

CREATE INDEX IF NOT EXISTS idx_sessions_root_history
    ON sessions(root_id, id);

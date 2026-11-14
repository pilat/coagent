-- +goose Up

ALTER TABLE messages DROP COLUMN retry_of_message_id;

ALTER TABLE session_budgets RENAME TO session_budgets_legacy;

CREATE TABLE session_budgets (
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
    CHECK (
        (state = 'armed'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NULL AND released_at IS NULL
            AND fired_reason = '' AND released_reason = '' AND observed_cost_usd IS NULL)
        OR (state = 'fired'
            AND (cost_limit_usd IS NOT NULL OR duration_seconds IS NOT NULL)
            AND fired_at IS NOT NULL AND released_at IS NULL
            AND fired_reason <> '' AND released_reason = '' AND observed_cost_usd IS NOT NULL)
        OR (state = 'released'
            AND released_at IS NOT NULL AND released_reason <> '')
    )
);

INSERT INTO session_budgets
    (root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd,
     duration_seconds, fired_at, released_at, fired_reason, released_reason, observed_cost_usd)
SELECT root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd,
       duration_seconds, fired_at, released_at, fired_reason, released_reason, observed_cost_usd
FROM session_budgets_legacy;

DROP TABLE session_budgets_legacy;

-- +goose Up

-- The last provider-measured context size, persisted across restarts so /status
-- and the compaction trigger keep using the provider's own count (D8). Zero
-- values mean nothing was measured; the model column guards the measurement
-- against model switches the way the in-memory modelEpoch does.
ALTER TABLE sessions ADD COLUMN context_baseline_model TEXT NOT NULL DEFAULT '';

ALTER TABLE sessions ADD COLUMN context_baseline_prompt_tokens INTEGER NOT NULL DEFAULT 0
    CHECK (context_baseline_prompt_tokens >= 0);

ALTER TABLE sessions ADD COLUMN context_baseline_message_count INTEGER NOT NULL DEFAULT 0
    CHECK (context_baseline_message_count >= 0);

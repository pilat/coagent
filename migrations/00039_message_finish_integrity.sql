-- +goose Up

ALTER TABLE messages ADD COLUMN finish_type TEXT
    CHECK (finish_type IS NULL OR finish_type IN ('stop', 'length', 'tool_calls', 'unknown'));
ALTER TABLE messages ADD COLUMN provider_finish_reason TEXT;
ALTER TABLE messages ADD COLUMN rejected_reason TEXT
	CHECK (rejected_reason IS NULL OR (
		role = 'assistant' AND finish_type IS NOT NULL AND (
            (finish_type = 'length' AND rejected_reason = 'output_length') OR
            (finish_type = 'unknown' AND rejected_reason = 'unknown_finish')
        )
    ));
ALTER TABLE messages ADD COLUMN retry_of_message_id INTEGER REFERENCES messages(id)
    CHECK (retry_of_message_id IS NULL OR role = 'user');

-- +goose Up
-- Root sessions were inserted without agent_type, so the schema default
-- ('general') made them resume as subagents: wrong prompt, no todo tools.
UPDATE sessions
SET agent_type = 'build'
WHERE (parent_id IS NULL OR parent_id = 0)
  AND (agent_type IS NULL OR agent_type = 'general');

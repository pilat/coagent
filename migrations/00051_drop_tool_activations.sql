-- +goose Up
DROP INDEX IF EXISTS idx_session_tool_activations_pending;
DROP TABLE session_tool_activations;

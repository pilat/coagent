package llmwire

import "encoding/json"

// ToolSchema is the wire description of a tool as sent to an LLM backend.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

package transcript

import (
	"encoding/json"
	"time"
)

// Message is one durable append-only conversation row.
type Message struct {
	ID               int64
	SessionID        int64
	Role             string
	Content          string
	ToolCallID       string
	ToolName         string
	ToolCalls        json.RawMessage
	ReasoningContent string
	ReasoningRaw     json.RawMessage
	Attachments      json.RawMessage
	CostUSD          float64
	Usage            json.RawMessage
	CompactedAt      *time.Time
	CreatedAt        time.Time
}

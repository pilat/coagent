package llmwire

import "encoding/json"

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// ContextInputFraction is the share of a model's context window conversation
// input may occupy; the complement is reserved for output (request max_tokens).
const ContextInputFraction = 0.85

// MessageUsage holds token usage for a single message.
//
// PromptTokens is the total input the model saw, cache included, uniformly across
// providers. CacheTokens (read) and CacheWriteTokens are subsets of it, so the
// invariant PromptTokens >= CacheTokens + CacheWriteTokens always holds
// (estimateCost relies on it). Anthropic's raw input_tokens excludes cache, so its
// extractor sums the read and write breakdowns back in.
type MessageUsage struct {
	PromptTokens     int `json:"promptTokens,omitempty"`
	CompletionTokens int `json:"completionTokens,omitempty"`
	CacheTokens      int `json:"cacheTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

type Message struct {
	DBID             int64  `json:"-"` // DB row ID for persistence; excluded from LLM serialization
	Role             string // "user", "assistant", "tool"
	Content          string
	ToolCallID       string
	ToolName         string
	ToolCalls        []ToolCall      // For assistant messages that call tools
	ReasoningContent string          // For OpenAI-compatible models that return reasoning_content
	ReasoningRaw     json.RawMessage `json:"ReasoningRaw,omitempty"` // sealed ReasoningEnvelope; replayed verbatim
	CostUSD          float64         `json:"CostUSD,omitempty"`
	Usage            *MessageUsage   `json:"Usage,omitempty"`
}

type Response struct {
	Text             string
	Thoughts         string // Model's reasoning/thinking (if available)
	ToolCalls        []ToolCall
	FinishType       string          // "stop", "tool_calls", "error"
	ReasoningContent string          // For OpenAI-compatible models that return reasoning_content
	ReasoningRaw     json.RawMessage // sealed ReasoningEnvelope; persisted and replayed verbatim
	CostUSD          float64
	Usage            *MessageUsage
}

// ToolCall uses explicit tags to pin its on-disk keys: the struct is persisted
// verbatim, so renaming a field must not silently orphan existing rows.
type ToolCall struct {
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	Arguments []byte `json:"Arguments"`
}

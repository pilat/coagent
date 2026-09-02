package llmwire

import "encoding/json"

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// Canonical image MIME set accepted end to end — read tool sniffing, Telegram
// ingestion advice and driver gating all map onto this one definition.
const (
	MimeImageJpeg = "image/jpeg"
	MimeImagePng  = "image/png"
	MimeImageGif  = "image/gif"
	MimeImageWebp = "image/webp"
)

// Reasons an image slot renders as text instead of pixels (D3 degradation);
// drivers share these so wording cannot drift between families.
const (
	ImageOmitReasonUnreadable  = "unreadable file"
	ImageOmitReasonUnsupported = "unsupported media type"
	ImageOmitReasonNoVision    = "model cannot accept images"
)

// ImagePlaceholder renders the inline text that replaces a degraded image slot.
func ImagePlaceholder(reason string) string {
	return "[image omitted: " + reason + "]"
}

// IsSupportedImageMime reports whether mime belongs to the canonical image set.
func IsSupportedImageMime(mime string) bool {
	switch mime {
	case MimeImageJpeg, MimeImagePng, MimeImageGif, MimeImageWebp:
		return true
	default:
		return false
	}
}

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

// ImageRef points at image bytes on disk. Explicit tags pin its on-disk keys:
// it is persisted verbatim in messages.attachments, so renaming a field must
// not silently orphan existing rows, and new fields must be absent-tolerant.
// Valid on RoleUser and RoleTool; other roles drop the field.
type ImageRef struct {
	Path string `json:"path"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	// Decoded pixel dimensions when the format is stdlib-decodable; zero on
	// rows written before they existed and on undecodable formats.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

type Message struct {
	Role       string // "user", "assistant", "tool"
	Content    string
	ToolCallID string
	ToolName   string
	// ToolError marks a durable typed failure on a tool result row; legacy
	// rows and ordinary results read as false.
	ToolError        bool
	ToolCalls        []ToolCall      // For assistant messages that call tools
	ReasoningContent string          // For OpenAI-compatible models that return reasoning_content
	ReasoningRaw     json.RawMessage `json:"ReasoningRaw,omitempty"` // sealed ReasoningEnvelope; replayed verbatim
	Images           []ImageRef      `json:"Images,omitempty"`       // referenced-not-stored attachments (tool results)
	CostUSD          float64         `json:"CostUSD,omitempty"`
	Usage            *MessageUsage   `json:"Usage,omitempty"`
}

// FinishType is the portable outcome vocabulary every driver reports on
// llmwire.Response. Native reasons that are neither normal completion, tool
// calling nor a length stop map to FinishUnknown rather than FinishStop.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
	FinishUnknown   = "unknown"
)

type Response struct {
	Text             string
	Thoughts         string // Model's reasoning/thinking (if available)
	ToolCalls        []ToolCall
	FinishType       string          // FinishStop, FinishToolCalls, FinishLength, FinishUnknown
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

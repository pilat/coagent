package llmwire

import "encoding/json"

// ChatOptions narrows a request or requests a supported capability.
// The client's configured limits and capabilities remain the ceiling.
type ChatOptions struct {
	// MaxTokens caps the response length of one request. 0 = no per-call cap.
	MaxTokens      int
	ResponseSchema *ResponseSchema
}

// ResponseSchema requests strict JSON output when the client supports it.
type ResponseSchema struct {
	Name   string
	Schema json.RawMessage
}

// ChatOption configures a single Chat request within the client's capabilities.
type ChatOption func(*ChatOptions)

// WithResponseSchema requests schema enforcement from capable clients only.
func WithResponseSchema(name string, schema json.RawMessage) ChatOption {
	return func(o *ChatOptions) { o.ResponseSchema = &ResponseSchema{Name: name, Schema: schema} }
}

// WithMaxTokens caps max_tokens for one request, so a brief cannot outgrow the
// room reserved for it however verbose the model feels.
func WithMaxTokens(n int) ChatOption {
	return func(o *ChatOptions) { o.MaxTokens = n }
}

// ApplyChatOptions resolves opts. The zero value means "unchanged": a caller
// passing nothing behaves exactly as before options existed.
func ApplyChatOptions(opts []ChatOption) ChatOptions {
	var resolved ChatOptions

	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}

	return resolved
}

// EffectiveMaxTokens combines a client's own limit with the per-call cap. A
// client limit of 0 means "unset", so the cap becomes the limit — min(0, cap)
// would silently apply nothing.
func (o ChatOptions) EffectiveMaxTokens(clientMax int) int {
	switch {
	case o.MaxTokens <= 0:
		return clientMax
	case clientMax <= 0:
		return o.MaxTokens
	default:
		return min(clientMax, o.MaxTokens)
	}
}

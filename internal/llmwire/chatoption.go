package llmwire

// ChatOptions is the resolved set of per-call narrowings. An option can only
// tighten what the client is already configured for, never widen it.
type ChatOptions struct {
	// MaxTokens caps the response length of one request. 0 = no per-call cap.
	MaxTokens int
}

// ChatOption narrows a single Chat request.
type ChatOption func(*ChatOptions)

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

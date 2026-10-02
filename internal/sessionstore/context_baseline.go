package sessionstore

// ContextBaseline is the last provider-measured context size, persisted across
// restarts. A measurement belongs to one model's window and tokenizer, so the
// model travels with the numbers.
type ContextBaseline struct {
	Model        string
	PromptTokens int
	MessageCount int
}

// ContextBaseline returns the persisted measurement, or nil when none was
// taken on any model.
func (r *SessionRecord) ContextBaseline() *ContextBaseline {
	if r.ContextBaselineModel == "" || r.ContextBaselinePromptTokens <= 0 {
		return nil
	}

	return &ContextBaseline{
		Model:        r.ContextBaselineModel,
		PromptTokens: r.ContextBaselinePromptTokens,
		MessageCount: r.ContextBaselineMessageCount,
	}
}

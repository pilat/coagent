package session

import (
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// compactionFraction is the share of the context window at which auto-compaction
// fires. The /status 🔴 band reuses it as its cutoff.
const compactionFraction = llmwire.ContextInputFraction

// contextBaseline is the last real measurement: the provider's own cache-
// inclusive PromptTokens and how many transcript messages it covered.
type contextBaseline struct {
	promptTokens int
	messageCount int
}

// estimateText is the len/4 rule applied to raw text.
func estimateText(text string) int {
	return len(text) / 4
}

// estimateTokens is a rough estimate of the conversation's own size — never
// persisted, never truth. Tool-call Arguments count: file bodies live there.
func estimateTokens(messages []llmwire.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Content) / 4
		total += len(msg.ReasoningContent) / 4

		for _, tc := range msg.ToolCalls {
			total += len(tc.Arguments) / 4
		}

		for _, ref := range msg.Images {
			// Providers downscale oversized images (long edge <=1568px), so the
			// real per-image cost stays near a few thousand tokens; raw Size/4
			// would over-count ordinary photos and trigger spurious compaction.
			total += min(int(ref.Size)/4, imageTokenCeiling)
		}
	}

	return total
}

// imageTokenCeiling bounds the estimator's per-image charge (D8).
const imageTokenCeiling = 8192

// estimateSchemas estimates the tool inventory a request carries.
func estimateSchemas(schemas []llmwire.ToolSchema) int {
	total := 0

	for _, schema := range schemas {
		total += estimateText(schema.Name) + estimateText(schema.Description) + len(schema.Parameters)/4
	}

	return total
}

// compactionCutoff is the projected size above which compaction fires.
func compactionCutoff(window int) int {
	return int(compactionFraction * float64(window))
}

// projectContextSize is the last measured number plus a len/4 estimate of
// everything appended since. A baseline the transcript shrank under is discarded.
func projectContextSize(messages []llmwire.Message, base *contextBaseline, overhead int) int {
	if base != nil && base.messageCount <= len(messages) {
		return base.promptTokens + estimateTokens(messages[base.messageCount:])
	}

	return estimateTokens(messages) + overhead
}

// requestOverhead is what a request carries besides the conversation. Only the
// unmeasured projection needs it — a measured baseline already includes it.
func (s *svc) requestOverhead() int {
	return estimateText(s.prompt.systemPrompt()) + estimateSchemas(tool.ToSchemas(s.registry.List()))
}

// projectContextSize reports the projection and whether it is a pure estimate
// (no provider measurement backing it).
func (s *svc) projectContextSize() (int, bool) {
	messages := s.ms.getMessages()
	overhead := s.requestOverhead()
	base := s.loadContextBaseline()

	return projectContextSize(messages, base, overhead), base == nil
}

func (s *svc) loadContextBaseline() *contextBaseline {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.baseline
}

// modelGeneration identifies the model a request is about to go out under.
func (s *svc) modelGeneration() uint64 {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.modelEpoch
}

// recordContextBaseline stores what the provider reported for a request covering
// sentCount messages. A model switch mid-flight discards it as another model's.
func (s *svc) recordContextBaseline(promptTokens, sentCount int, generation uint64) {
	if promptTokens <= 0 {
		return
	}

	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	if s.modelEpoch != generation {
		return
	}

	s.baseline = &contextBaseline{promptTokens: promptTokens, messageCount: sentCount}
}

// resetContextBaseline drops back to pure estimation.
func (s *svc) resetContextBaseline() {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	s.baseline = nil
}

// shouldCompact reports whether the projected request size exceeds
// compactionFraction of the window.
func (s *svc) shouldCompact(window int) bool {
	size, _ := s.projectContextSize()

	return size > compactionCutoff(window)
}

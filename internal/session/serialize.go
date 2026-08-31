package session

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
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
			total += imageTokenEstimate(ref)
		}
	}

	return total
}

// imageTokenEstimate charges the measured 28-pixel patch quantum — exact for
// the incident model and the best proxy for every other cataloged vision
// model; file size, the previous basis, provably does not affect image cost.
func imageTokenEstimate(ref llmwire.ImageRef) int {
	if ref.Width > 0 && ref.Height > 0 {
		q := imagePatchQuantumPx

		return ((ref.Width + q - 1) / q) * ((ref.Height + q - 1) / q)
	}

	// Without dimensions, size capped: providers downscale oversized images,
	// so raw Size/4 would over-count ordinary photos.
	return min(int(ref.Size)/4, imageTokenCeiling)
}

// imageTokenCeiling bounds the estimator's per-image charge when dimensions
// are absent.
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
// The measurement also persists best-effort: a failed write costs /status its
// accuracy across a restart, never the turn.
func (s *svc) recordContextBaseline(ctx context.Context, promptTokens, sentCount int, generation uint64) {
	if promptTokens <= 0 {
		return
	}

	model, ok := s.storeContextBaseline(promptTokens, sentCount, generation)
	if !ok {
		return
	}

	if s.store == nil {
		return
	}

	err := s.store.SaveContextBaseline(ctx, s.id, sessionstore.ContextBaseline{
		Model:        model,
		PromptTokens: promptTokens,
		MessageCount: sentCount,
	})
	if err != nil {
		logger.Ctx(ctx).Named("session.context").Warn("persist_context_baseline_failed", zap.Error(err))
	}
}

// storeContextBaseline installs the measurement in memory when it describes the
// current model generation, returning the model to persist it under.
func (s *svc) storeContextBaseline(promptTokens, sentCount int, generation uint64) (string, bool) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	if s.modelEpoch != generation {
		return "", false
	}

	s.baseline = &contextBaseline{promptTokens: promptTokens, messageCount: sentCount}

	return s.model, true
}

// clearPersistedBaseline drops the stored measurement best-effort. It must run
// wherever the transcript the measurement described is replaced: otherwise a
// crash before the next successful response resurrects a stale baseline whose
// message-count guard passes on equality.
func (s *svc) clearPersistedBaseline(ctx context.Context) {
	if s.store == nil {
		return
	}

	if err := s.store.ClearContextBaseline(ctx, s.id); err != nil {
		logger.Ctx(ctx).Named("session.context").Warn("clear_context_baseline_failed", zap.Error(err))
	}
}

// installPersistedBaseline adopts the last measurement across a restart. It is
// discarded when the session's current model differs — a measurement describes
// one model's window and tokenizer, the same rule the in-memory modelEpoch
// encodes for mid-flight switches.
func (s *svc) installPersistedBaseline(b *sessionstore.ContextBaseline) {
	if b == nil || b.Model != s.model || b.PromptTokens <= 0 {
		return
	}

	s.baseline = &contextBaseline{promptTokens: b.PromptTokens, messageCount: b.MessageCount}
}

// resetContextBaseline drops back to pure estimation.
func (s *svc) resetContextBaseline() {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	s.baseline = nil
}

// hasCompactionCandidate reports whether the raw range can yield a checkpoint
// split at all. The tail is never empty (D3), so a transcript whose only legal
// group would have to stay verbatim has nothing to summarize — the automatic
// path must not announce an attempt it can never make. The same head-fit bound
// compactLocked applies is included, so the pre-check and the authoritative
// re-selection inside compact() agree.
func (s *svc) hasCompactionCandidate(window int) bool {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	headerSize := compactionHeaderSize(s.ms.messages)

	cp := parseCheckpointPrefix(s.ms.messages, headerSize)
	if cp.rawStart >= len(s.ms.messages) {
		return false
	}

	headerJSONL, err := serializeCanonical(s.ms.messages[:headerSize])
	if err != nil {
		return false
	}

	baseEstimate := s.summarizerBaseEstimateLocked(headerJSONL, cp.prevSummary)
	minTail := minTailTokens(s.ms.messages, cp.rawStart, window)

	for _, limit := range tailLevels() {
		if _, ok := selectTailSplit(s.ms.messages, cp.rawStart, minTail, baseEstimate, window, limit); ok {
			return true
		}
	}

	return false
}

// shouldCompact reports whether the projected request size exceeds
// compactionFraction of the window, or image pressure breaches a high-water
// mark (D1/D5): a byte wall the token projection cannot see.
func (s *svc) shouldCompact(window int) bool {
	size, _ := s.projectContextSize()
	if size > compactionCutoff(window) {
		return true
	}

	totalBytes, count := imagePressure(s.ms.getMessages())

	return totalBytes > imageBytesHighWater || count > imageCountHighWater
}

package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

var (
	// errCompactionPendingCall guards data, not politeness: compacting a transcript
	// that still owes a tool_use its result orphans the producer waiting on it.
	errCompactionPendingCall = errors.New("compaction refused: a tool call is still awaiting its result")

	// errCompactionHeaderTooLarge: no summary can help, the untouchable header
	// alone is over the trigger.
	errCompactionHeaderTooLarge = errors.New(
		"project context and system prompt alone exceed the compaction threshold",
	)

	// errNothingToCompact: no raw head group can be summarized at this pressure.
	errNothingToCompact = errors.New("nothing to compact")

	// errCompactionNonRelieving: the candidate checkpoint would still leave the
	// next ordinary request above the trigger, so it is refused whole.
	errCompactionNonRelieving = errors.New(
		"compaction rejected: the resulting projection stays above the pressure threshold",
	)
)

// compactionHeaderTooLargeNotice is what the human sees when no summary can
// help: the untouchable header alone is over the trigger.
const compactionHeaderTooLargeNotice = "⚠️ Project context and system prompt alone exceed the compaction " +
	"threshold for this model — switch to a model with a larger context window."

// compactionUsage sums the LLM cost/usage one compact() spends across its
// summarizer call, so the summary row carries compaction's own real cost.
type compactionUsage struct {
	cost  float64
	usage llmwire.MessageUsage
}

func (a *compactionUsage) add(resp *llmwire.Response) {
	if resp == nil {
		return
	}

	a.cost += resp.CostUSD

	if resp.Usage != nil {
		a.usage.PromptTokens += resp.Usage.PromptTokens
		a.usage.CompletionTokens += resp.Usage.CompletionTokens
		a.usage.CacheTokens += resp.Usage.CacheTokens
		a.usage.CacheWriteTokens += resp.Usage.CacheWriteTokens
	}
}

// focusSection renders the optional /compact focus as a prompt section, or "" when
// no focus is set (bare /compact and every auto-compaction).
func (s *svc) focusSection() string {
	if s.compactionFocus == "" {
		return ""
	}

	return "\n\nPriority for this summary: " + s.compactionFocus
}

// compact builds one checkpoint candidate and, only when every candidate check
// passes, commits it as one atomic positioned replacement. A failed or
// non-relieving attempt changes no active transcript metadata.
func (s *svc) compact(ctx context.Context, commandInput *PendingInput) (bool, error) {
	// Defence in depth: a caller that forgets the gate must not compact a
	// transcript that still owes a tool_use its result.
	if s.HasPendingExternalCall() || s.HasPendingWork() {
		return false, errCompactionPendingCall
	}

	// Read the ledger before taking the transcript lock: the provider does IO.
	background := s.activeBackgroundSection(ctx)

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	return s.compactLocked(ctx, background, commandInput)
}

// compactLocked is compact's transcript-mutating half, under s.ms.mu.
func (s *svc) compactLocked(
	ctx context.Context,
	background string,
	commandInput *PendingInput,
) (bool, error) {
	log := logger.Ctx(ctx).Named("session.compaction")

	headerSize := compactionHeaderSize(s.ms.messages)
	if err := validateCompactionHeader(s.ms.messages[:headerSize]); err != nil {
		return false, err
	}

	if !s.headerFitsLocked(headerSize) {
		return false, errCompactionHeaderTooLarge
	}

	cp := parseCheckpointPrefix(s.ms.messages, headerSize)
	window := s.contextWindow()

	candIdx, candEnvelope := selectCurrentSkill(s.ms.messages, headerSize, cp.summaryRowIdx)

	split, summaryMsg, err := s.buildCheckpointCandidate(ctx, cp, background, window)
	if err != nil {
		if errors.Is(err, errNothingToCompact) {
			return false, nil
		}

		return false, err
	}

	beforeCount := len(s.ms.messages)

	newMessages, compactedIDs := s.assembleCheckpointLocked(cp, headerSize, split, candIdx, candEnvelope, summaryMsg)

	// The candidate must actually relieve the pressure, equality included
	// (shouldCompact fires on strict greater-than); otherwise nothing is written.
	if size := estimateTokens(newMessages) + s.requestOverhead(); size > compactionCutoff(window) {
		return false, errCompactionNonRelieving
	}

	if err := s.commitCheckpointLocked(ctx, newMessages, compactedIDs, commandInput); err != nil {
		return false, err
	}

	s.resetContextBaseline() // the transcript the measurement described is gone

	if commandInput == nil {
		s.compactionSummaryDBID = newMessages[headerSize].DBID
	}

	s.logCompactionLocked(log, beforeCount, len(newMessages), split-cp.rawStart, cp, summaryMsg)

	return true, nil
}

func (s *svc) logCompactionLocked(
	log *zap.Logger,
	beforeMessages, afterMessages, summarized int,
	cp checkpointPrefix,
	summaryMsg llmwire.Message,
) {
	log.Info("compaction_completed",
		zap.Int("before_messages", beforeMessages),
		zap.Int("after_messages", afterMessages),
		zap.Int("summarized", summarized),
		zap.Int("summary_len", len(summaryMsg.Content)),
		zap.Bool("incremental", cp.summaryRowIdx >= 0),
	)
}

// buildCheckpointCandidate selects the split, runs the summarizer and wraps the
// marked summary row. Caller holds s.ms.mu.
func (s *svc) buildCheckpointCandidate(
	ctx context.Context,
	cp checkpointPrefix,
	background string,
	window int,
) (int, llmwire.Message, error) {
	headerRef, err := serializeCanonical(s.ms.messages[:compactionHeaderSize(s.ms.messages)])
	if err != nil {
		return 0, llmwire.Message{}, err
	}

	baseEstimate := s.summarizerBaseEstimateLocked(headerRef, cp.prevSummary)

	split, headJSONL, err := s.selectAndSerializeHeadLocked(cp, window, baseEstimate)
	if err != nil {
		return 0, llmwire.Message{}, err
	}

	summaryText, acc, err := s.summarizeCheckpoint(ctx, headerRef, cp.prevSummary, headJSONL, window)
	if err != nil {
		return 0, llmwire.Message{}, fmt.Errorf("compaction failed: %w", err)
	}

	summaryMsg := llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: renderMarkedSummary(summaryText, background),
		CostUSD: acc.cost,
		Usage:   &acc.usage,
	}

	return split, summaryMsg, nil
}

// selectAndSerializeHeadLocked picks the split, builds the canonical head and
// validates both the raw cut and the post-exclusion projection. Returns
// ( "", nil ) only via the nothing-to-compact path.
func (s *svc) selectAndSerializeHeadLocked(cp checkpointPrefix, window, baseEstimate int) (int, string, error) {
	split, ok := selectCheckpointSplit(s.ms.messages, cp, baseEstimate, window)
	if !ok {
		return 0, "", errNothingToCompact
	}

	head := canonicalHead(s.ms.messages, cp, split)

	// The sanitized projection is what the checkpoint serializes; validating
	// the raw head here would reject aborted turns sanitizeIncompleteCalls
	// exists to strip.
	if err := validateRawHead(head); err != nil {
		return 0, "", fmt.Errorf("compaction head is not provider-valid: %w", err)
	}

	headJSONL, err := serializeCanonical(head)
	if err != nil {
		return 0, "", err
	}

	return split, headJSONL, nil
}

// commitCheckpointLocked persists the replacement in the transaction the
// situation demands — budgeted, command-settling, or plain — then adopts the
// new projection in memory. Caller holds s.ms.mu.
func (s *svc) commitCheckpointLocked(
	ctx context.Context,
	newMessages []llmwire.Message,
	compactedIDs []int64,
	commandInput *PendingInput,
) error {
	entries, err := compactionEntries(newMessages)
	if err != nil {
		return err
	}

	switch {
	case s.budgetGate != nil:
		inputID := int64(0)
		if commandInput != nil {
			inputID = commandInput.ID
		}

		ids, fired, persistErr := s.budgetGate.PersistCompaction(ctx, sessionstore.BudgetedCompaction{
			InputID: inputID, CompactedIDs: compactedIDs, Entries: entries,
			ObservedAt: time.Now().UTC(),
		})
		if persistErr != nil {
			return fmt.Errorf("replace budgeted compacted messages: %w", persistErr)
		}

		if err := stampCompactionIDs(newMessages, ids); err != nil {
			return err
		}

		s.budgetFired = fired
	case commandInput != nil:
		if err := s.ms.completeCompactionCommandLocked(ctx, *commandInput, compactedIDs, newMessages); err != nil {
			return err
		}
	default:
		if err := s.ms.replaceCompactedMessagesLocked(ctx, compactedIDs, newMessages); err != nil {
			return fmt.Errorf("replace compacted messages: %w", err)
		}
	}

	s.ms.messages = newMessages

	return s.publishCompactionProgress(ctx)
}

// stampCompactionIDs adopts the store's returned row IDs into the in-memory
// projection, in entry order.
func stampCompactionIDs(messages []llmwire.Message, ids []int64) error {
	if len(ids) != len(messages) {
		return fmt.Errorf("budgeted compaction returned %d ids for %d messages", len(ids), len(messages))
	}

	for i := range messages {
		messages[i].DBID = ids[i]
	}

	return nil
}

// publishCompactionProgress publishes the post-compaction operator snapshot.
// A session without an output owner (hermetic tests) tolerates the owner error.
func (s *svc) publishCompactionProgress(ctx context.Context) error {
	provider, ok := s.boundary.(progressChangeBoundary)
	if !ok {
		return nil
	}

	_, published, err := provider.ProgressChange(ctx)
	if err != nil {
		if errors.Is(err, sessionstore.ErrOutputOwner) ||
			errors.Is(err, sessionstore.ErrProgressSuperseded) {
			return nil
		}

		return fmt.Errorf("enqueue compaction progress snapshot: %w", err)
	}

	_ = published

	return nil
}

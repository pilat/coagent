package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const (
	// compactionOutputReserve is the window room a summarization call keeps for
	// the brief it writes, and the per-call max_tokens cap that holds it there.
	compactionOutputReserve = 8000

	compactionSummaryPrefix = "[CONTEXT SUMMARY"
	compactionPrimerPrefix  = "[Post-compaction"

	// compactionHeaderTooLargeNotice is what the human sees when no summary can
	// help: the untouchable header alone is over the trigger.
	compactionHeaderTooLargeNotice = "⚠️ Project context and system prompt alone exceed the compaction " +
		"threshold for this model — switch to a model with a larger context window."
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
)

// compactionUsage sums the LLM cost/usage one compact() spends across every Chat
// call it fans out to, so the summary row can carry compaction's own real cost.
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

// compact replaces everything after the header with a brief and reports whether
// the transcript was restructured. keepRecent now only bounds how many recent
// rounds the summarizer reads with their tool output intact.
func (s *svc) compact(ctx context.Context, keepRecent int) (bool, error) {
	return s.compactWithCommand(ctx, keepRecent, nil)
}

func (s *svc) compactWithCommand(
	ctx context.Context,
	keepRecent int,
	commandInput *PendingInput,
) (bool, error) {
	log := logger.Ctx(ctx).Named("session.compaction")

	// Defence in depth: a caller that forgets the gate must not delete a tool_use
	// an external producer still owns.
	if s.HasPendingExternalCall() || s.HasPendingWork() {
		return false, errCompactionPendingCall
	}

	// Tool bodies are the bulk of a transcript and the summarizer never needed
	// them verbatim; dropping them is what makes one prompt affordable.
	if n := s.applyClear(ctx, keepRecent); n > 0 {
		log.Info("compaction_cleared_tool_results", zap.Int("cleared", n))
		// Clearing shrinks content without changing the message count, so the
		// measured number would keep projecting the pre-clear size.
		s.resetContextBaseline()
	}

	// Read the ledger before taking the transcript lock: the provider does IO.
	background := s.activeBackgroundSection(ctx)

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	return s.compactLocked(ctx, log, background, commandInput)
}

// compactLocked is compact's transcript-mutating half, under s.ms.mu.
func (s *svc) compactLocked(
	ctx context.Context,
	log *zap.Logger,
	background string,
	commandInput *PendingInput,
) (bool, error) {
	headerSize := compactionHeaderSize(s.ms.messages)
	if !s.headerFitsLocked(headerSize) {
		return false, errCompactionHeaderTooLarge
	}

	summarizeEnd := len(s.ms.messages)

	summarizeStart := summarizeStartAfter(s.ms.messages, headerSize)
	if summarizeEnd <= summarizeStart {
		return false, nil
	}

	// Candidates span everything being replaced, not just what the summarizer
	// reads: a skill the previous compaction reattached is deleted by this one too.
	reattachments := selectSkillReattachments(s.ms.messages, headerSize, summarizeEnd, s.contextWindow())

	acc := &compactionUsage{}

	brief, isIncremental, err := s.summarizeLocked(ctx, summarizeStart, acc)
	if err != nil {
		return false, fmt.Errorf("compaction failed: %w", err)
	}

	turn := summaryTurn{
		brief:      brief,
		verbatim:   buildVerbatimTail(s.ms.messages[summarizeStart:]),
		background: background,
	}

	// The brief advances only once the durable swap landed: describing messages
	// still in the transcript would summarize the same work twice.
	beforeCount, err := s.rebuildMessages(ctx, turn, acc, headerSize, reattachments, brief, commandInput)
	if err != nil {
		return false, err
	}

	s.resetContextBaseline() // the transcript the measurement described is gone
	s.compactionBrief = brief

	if commandInput == nil {
		// rebuildMessages left the summary turn right after the header.
		s.compactionSummaryDBID = s.ms.messages[headerSize].DBID

		if err := s.ms.persistCompactionBrief(ctx, brief); err != nil {
			log.Warn("persist_compaction_brief_failed", zap.Error(err))
		}
	}

	log.Info("compaction_completed",
		zap.Int("before_messages", beforeCount),
		zap.Int("after_messages", len(s.ms.messages)),
		zap.Int("summarized", summarizeEnd-summarizeStart),
		zap.Int("brief_len", len(brief)),
		zap.Bool("incremental", isIncremental),
	)

	return true, nil
}

func compactionHeaderSize(messages []llmwire.Message) int {
	if len(messages) == 0 {
		return 0
	}

	if messages[0].Role == llmwire.RoleSystem || strings.HasPrefix(messages[0].Content, agentsMDMessagePrefix) {
		return min(2, len(messages))
	}

	return 1
}

// summarizeStartAfter skips what a previous compaction wrote, so a second one
// finds only new work instead of re-summarizing its own output.
func summarizeStartAfter(messages []llmwire.Message, headerSize int) int {
	start := headerSize

	if start >= len(messages) || !strings.HasPrefix(messages[start].Content, compactionSummaryPrefix) {
		return start
	}

	start++

	if start < len(messages) && messages[start].Role == llmwire.RoleAssistant &&
		messages[start].Content == registry.PostCompactionAssistantAck {
		start++
	}

	if start < len(messages) && strings.HasPrefix(messages[start].Content, compactionPrimerPrefix) {
		start++
	}

	for start < len(messages) && messages[start].Role == llmwire.RoleUser &&
		len(builtin.ExtractRenderedSkills(messages[start].Content)) > 0 {
		start++
	}

	return start
}

// headerFitsLocked reports whether compaction can converge at all: the header is
// never summarized and the system prompt rides along on every request.
func (s *svc) headerFitsLocked(headerSize int) bool {
	size := estimateTokens(s.ms.messages[:headerSize]) + estimateText(s.prompt.systemPrompt())

	return size <= compactionCutoff(s.contextWindow())
}

// summarizeLocked produces the brief, reporting whether the incremental path ran.
// It reads the WHOLE conversation: a brief written without the opening task and
// the current tail can say neither where the work started nor where it stands.
func (s *svc) summarizeLocked(ctx context.Context, summarizeStart int, acc *compactionUsage) (string, bool, error) {
	if s.compactionBrief == "" {
		brief, err := s.compactInitialLocked(ctx, s.ms.messages, acc)

		return brief, false, err
	}

	brief, err := s.compactMergeLocked(ctx, s.compactionBrief, s.ms.messages[summarizeStart:], acc)

	return brief, true, err
}

// rebuildMessages replaces everything after the header with the summary turn and
// the skill reattachments, and returns the pre-rebuild message count for logging.
func (s *svc) rebuildMessages(
	ctx context.Context,
	turn summaryTurn,
	acc *compactionUsage,
	headerSize int,
	reattachments []llmwire.Message,
	brief string,
	commandInput *PendingInput,
) (int, error) {
	beforeCount := len(s.ms.messages)
	compactedIDs := make([]int64, 0, beforeCount-headerSize)

	for _, message := range s.ms.messages[headerSize:] {
		if message.DBID != 0 {
			compactedIDs = append(compactedIDs, message.DBID)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	newMessages := make([]llmwire.Message, 0, headerSize+3+len(reattachments))
	newMessages = append(newMessages, s.ms.messages[:headerSize]...)

	// Summary row carries compaction's OWN cost/usage; the replaced originals keep
	// theirs, so a lifetime tree-sum counts each exactly once (no rollup double-count).
	summaryUsage := acc.usage
	summaryMsg := llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: turn.render(),
		CostUSD: acc.cost,
		Usage:   &summaryUsage,
	}

	ackMsg := llmwire.Message{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck}
	primerMsg := llmwire.Message{Role: llmwire.RoleUser, Content: fmt.Sprintf(registry.PostCompactionPrimer, now)}

	newMessages = append(newMessages, summaryMsg, ackMsg, primerMsg)
	newMessages = append(newMessages, reattachments...)

	//nolint:nestif,wsl_v5 // Atomic and ordinary persistence share the assembled projection.
	if s.budgetGate != nil {
		entries, err := compactionEntries(newMessages)
		if err != nil {
			return 0, err
		}
		inputID := int64(0)
		if commandInput != nil {
			inputID = commandInput.ID
		}
		ids, fired, err := s.budgetGate.PersistCompaction(ctx, sessionstore.BudgetedCompaction{
			InputID: inputID, CompactedIDs: compactedIDs, Entries: entries, Brief: brief,
			ObservedAt: time.Now().UTC(),
		})
		if err != nil {
			return 0, fmt.Errorf("replace budgeted compacted messages: %w", err)
		}
		if len(ids) != len(newMessages) {
			return 0, fmt.Errorf("budgeted compaction returned %d ids for %d messages", len(ids), len(newMessages))
		}
		for i := range newMessages {
			newMessages[i].DBID = ids[i]
		}
		s.budgetFired = fired
	} else if err := s.ms.replaceCompactedMessagesWithCommandLocked(
		ctx, compactedIDs, newMessages, brief, commandInput,
	); err != nil {
		return 0, fmt.Errorf("replace compacted messages: %w", err)
	}

	s.ms.messages = newMessages
	//nolint:nestif // Optional progress failure has an ownerless-root no-op branch.
	if !s.budgetFired {
		if provider, ok := s.boundary.(progressChangeBoundary); ok {
			if _, err := provider.ProgressChange(ctx); err != nil {
				if errors.Is(err, sessionstore.ErrOutputOwner) {
					return beforeCount, nil
				}

				return 0, fmt.Errorf("enqueue compaction progress snapshot: %w", err)
			}
		}
	}

	return beforeCount, nil
}

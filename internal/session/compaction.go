package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// compactionHeaderTooLargeNotice is what the human sees when no summary can
// help: the untouchable header alone is over the trigger.
const compactionHeaderTooLargeNotice = "⚠️ Project context and system prompt alone exceed the compaction " +
	"threshold for this model — switch to a model with a larger context window."

const compactionNotConvergingNotice = "⚠️ Context window too small for this workload — compaction is no " +
	"longer freeing enough space. Automatic compaction is paused for this run; switch to a model with a " +
	"larger context window."

const (
	// legacyBackgroundSectionMarker keeps pre-rename checkpoints readable.
	legacyBackgroundSectionMarker = "\n\n# Active subagents\n"
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
func (s *Session) focusSection() string {
	if s.compactionFocus == "" {
		return ""
	}

	return "\n\nPriority for this summary: " + s.compactionFocus
}

// compact builds one checkpoint candidate and, only when every candidate check
// passes, commits it as one atomic positioned replacement. A failed or
// non-relieving attempt changes no active transcript metadata.
func (s *Session) compact(ctx context.Context, commandInput *PendingInput) (bool, error) {
	// Defence in depth: a caller that forgets the gate must not compact a
	// transcript that still owes a tool_use its result.
	if s.HasPendingExternalCall() || len(s.pendingInLoopCalls()) > 0 {
		return false, errCompactionPendingCall
	}

	// Read the ledger and snapshot the transcript before taking the transcript
	// lock: the provider does IO, and repair reads ms.mu through
	// pendingExternalCallIDs — calling it under the lock would deadlock.
	background := s.activeBackgroundSection(ctx)
	completionPin, completionPending := s.completionCompactionPin(ctx)

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	return s.compactLocked(ctx, background, commandInput, completionPin, completionPending)
}

// completionCompactionPin resolves the pending candidate's transcript position
// the split search may never summarize past: candidate and nudge stay verbatim
// tail rows until the check resolves. The second result reports that a durable
// check is pending at all, so a candidate the live transcript cannot place
// keeps compaction non-relieving instead of relaxing the tail cap.
func (s *Session) completionCompactionPin(ctx context.Context) (int, bool) {
	if s.store == nil {
		return 0, false
	}

	state, err := s.store.LoadCompletionCheckState(ctx, s.id)
	if err != nil || state == nil || state.CandidateID == nil {
		return 0, false
	}

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	for i, rowID := range s.ms.rowIDs {
		if rowID == *state.CandidateID {
			return i, true
		}
	}

	return 0, true
}

// compactLocked is compact's transcript-mutating half, under s.ms.mu.
func (s *Session) compactLocked(
	ctx context.Context,
	background string,
	commandInput *PendingInput,
	completionPin int,
	completionPending bool,
) (bool, error) {
	log := logger.Ctx(ctx).Named("session.compaction")

	// A pending check whose candidate the live transcript cannot place cannot
	// retain the verbatim pair, so the attempt stays non-relieving instead of
	// summarizing the evidence being confirmed.
	if completionPending && completionPin == 0 {
		log.Warn("completion_pin_unresolved", zap.Int64("session_id", s.id))
		return false, nil
	}

	// The loop gate already refuses pending calls; the snapshot must match the
	// ordinary request's repair, which excludes genuinely-pending external calls.
	pendingExternal := s.pendingExternalCallIDsLocked(s.ms.messages)

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

	split, summaryMsg, err := s.buildCheckpointCandidate(ctx, cp, background, pendingExternal, window, completionPin)
	if err != nil {
		if errors.Is(err, errNothingToCompact) {
			return false, nil
		}

		return false, err
	}

	beforeCount := len(s.ms.messages)

	newMessages, newRowIDs, compactedIDs := s.assembleCheckpointLocked(
		cp, headerSize, split, candIdx, candEnvelope, summaryMsg,
	)

	// The candidate must actually relieve the pressure, equality included
	// (shouldCompact fires on strict greater-than); otherwise nothing is written.
	if size := estimateTokens(newMessages) + s.requestOverhead(); size > compactionCutoff(window) {
		return false, errCompactionNonRelieving
	}

	if totalBytes, count := imagePressure(
		newMessages,
	); totalBytes > imageBytesHighWater ||
		count > imageCountHighWater {
		return false, errCompactionNonRelieving
	}

	if err := s.commitCheckpointLocked(ctx, newMessages, newRowIDs, compactedIDs, commandInput); err != nil {
		return false, err
	}

	s.resetContextBaseline() // the transcript the measurement described is gone

	if commandInput == nil {
		s.compactionSummaryDBID = newRowIDs[headerSize]
	}

	s.logCompactionLocked(log, beforeCount, len(newMessages), split-cp.rawStart, cp, summaryMsg)

	return true, nil
}

func (s *Session) logCompactionLocked(
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
		zap.Bool("repeated", cp.summaryRowIdx >= 0),
	)
}

// buildCheckpointCandidate selects the split, runs the summarizer and wraps the
// marked summary row. Caller holds s.ms.mu.
func (s *Session) buildCheckpointCandidate(
	ctx context.Context,
	cp checkpointPrefix,
	background string,
	pendingExternal map[string]bool,
	window int,
	completionPin int,
) (int, llmwire.Message, error) {
	split, ok := selectCheckpointSplit(s.ms.messages, cp, s.summarizerBaseEstimateLocked(), window, completionPin)
	if !ok {
		return 0, llmwire.Message{}, errNothingToCompact
	}

	summaryText, acc, err := s.summarizeCheckpoint(ctx, split, pendingExternal, window)
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

// commitCheckpointLocked persists the replacement in the transaction the
// situation demands — budgeted, command-settling, or plain — then adopts the
// new projection in memory. Caller holds s.ms.mu.
func (s *Session) commitCheckpointLocked(
	ctx context.Context,
	newMessages []llmwire.Message,
	newRowIDs []int64,
	compactedIDs []int64,
	commandInput *PendingInput,
) error {
	entries, err := compactionEntries(newMessages, newRowIDs)
	if err != nil {
		return err
	}

	c := s.newCommit()
	c.Replace = &sessionstore.Replace{HeadIDs: compactedIDs, Entries: entries}
	c.ObserveBudget = true

	c.State.ClearContextBaseline = true
	if commandInput != nil {
		c.Accept = []sessionstore.Accept{
			{InputID: commandInput.ID, State: sessionstore.InputStateHandled, Reason: "compact command", LinkRef: -1},
		}
		c.Outputs = []sessionstore.Output{
			{
				Type:          sessionstore.OutputMessagePersistent,
				Content:       "✅ Context compacted",
				Key:           fmt.Sprintf("input:%d:compact:succeeded", commandInput.ID),
				MessageRef:    -1,
				ReleasesInput: true,
			},
		}
	}

	result, err := s.store.Commit(ctx, c)
	if err != nil {
		return fmt.Errorf("commit checkpoint locked: %w", err)
	}

	s.budgetFired = result.BudgetFired

	s.compactionOutputs = s.liveOutputs(c, result)
	if err := s.ms.reloadMessagesLocked(ctx); err != nil {
		return err
	}

	copy(newRowIDs, s.ms.rowIDs)

	return nil
}

// applyContextEvents is the single sanctioned compaction point: an explicit
// request forces, otherwise the projected request size decides.
//
//nolint:gocyclo,nestif,funlen // Explicit compaction has a durable start, terminal outcome, and auto-path fallback.
func (s *Session) compactionStep(ctx context.Context, r *runState) error {
	if s.HasPendingExternalCall() || len(s.pendingInLoopCalls()) > 0 {
		return nil
	}

	command := s.compactionCommandInput()
	explicit := s.consumePendingCompaction()

	window := s.contextWindow()
	if !explicit && (r.autoCompactionOff || !s.shouldCompact(window) || !s.hasCompactionCandidate(window)) {
		return nil
	}

	fired, err := s.observeBudget(ctx)
	if err != nil {
		return err
	}

	if fired {
		s.budgetFired = true
		if command != nil {
			return s.finishCompactionCommand(
				ctx,
				*command,
				"parked",
				"⏸ Budget checkpoint reached — the session is parked. Send a message to resume.",
			)
		}

		return nil
	}

	c := s.newCommit()

	c.Outputs = []sessionstore.Output{
		{Type: sessionstore.OutputMessageReplaceable, Content: "🔄 Compacting context...", MessageRef: -1},
	}
	if command != nil {
		c.Outputs[0].Key = fmt.Sprintf("input:%d:compact:started", command.ID)
	}

	if _, err := s.commit(ctx, c); err != nil {
		return err
	}

	ok, compactErr := s.compact(ctx, command)
	s.emitCommitted(s.compactionOutputs, s.budgetFired)
	s.compactionOutputs = nil
	s.setCompactionFocus("")

	terminal := ""

	switch {
	case errors.Is(compactErr, errCompactionHeaderTooLarge):
		terminal = compactionHeaderTooLargeNotice
	case compactErr != nil:
		terminal = "❌ Compaction failed"
	case ok:
		terminal = "✅ Context compacted"
	case explicit:
		terminal = "Nothing to compact"
	}

	if command != nil {
		if !ok {
			if err := s.finishCompactionCommand(
				ctx,
				*command,
				compactionOutcomePhase(ok, compactErr),
				terminal,
			); err != nil {
				return err
			}
		}

		s.clearCompactionCommandInput()
	} else if terminal != "" {
		c := s.newCommit()

		c.Outputs = []sessionstore.Output{
			{Type: sessionstore.OutputMessagePersistent, Content: terminal, MessageRef: -1},
		}
		if ok {
			c.Outputs[0].Key = fmt.Sprintf("compaction:%d:succeeded", s.compactionSummaryDBID)
		}

		if _, err := s.commit(ctx, c); err != nil {
			return err
		}
	}

	if !explicit {
		if ok && compactErr == nil && !s.shouldCompact(window) {
			r.compactionFailures = 0
		} else {
			r.compactionFailures++
		}

		if r.compactionFailures >= compactionAttemptCap {
			r.autoCompactionOff = true
			c := s.newCommit()

			c.Outputs = []sessionstore.Output{
				{Type: sessionstore.OutputMessagePersistent, Content: compactionNotConvergingNotice, MessageRef: -1},
			}
			if _, err := s.commit(ctx, c); err != nil {
				return err
			}
		}
	}

	return nil
}

func compactionOutcomePhase(ok bool, err error) string {
	if ok {
		return "succeeded"
	}

	if err == nil {
		return "nothing"
	}

	return "failed"
}

func (s *Session) finishCompactionCommand(ctx context.Context, input PendingInput, phase, content string) error {
	c := s.newCommit()
	c.Accept = []sessionstore.Accept{
		{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "compact command", LinkRef: -1},
	}
	c.Outputs = []sessionstore.Output{
		{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       content,
			Key:           fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
			MessageRef:    -1,
			ReleasesInput: true,
		},
	}
	_, err := s.commit(ctx, c)

	return err
}

// summarizeCheckpoint runs the one no-tools model call that produces the
// checkpoint: the ordinary repaired prefix from the transcript beginning
// through the split, the same system prompt, schemas and tool-choice behavior
// an ordinary request carries, and one final role-user instruction. A model
// that answers a tool call instead of text gets one tools-unavailable nudge
// and must then answer in plain text. A failed attempt persists no boundary
// and may submit the same head again on a later attempt.
func (s *Session) summarizeCheckpoint(
	ctx context.Context,
	split int,
	pendingExternal map[string]bool,
	window int,
) (string, *compactionUsage, error) {
	activeTools := s.registry.List()
	if s.loopDetector.forceTextOnly {
		activeTools = nil
	}

	schemas := tool.ToSchemas(activeTools)

	messages := append(
		repairTranscriptExcluding(s.ms.messages[:split], pendingExternal),
		compactionInstructionMessage(s.focusSection()),
	)

	// The normal full output reserve: the ordinary complement of the input
	// fraction, not a summary-length target — any useful completed length passes.
	reserve := int((1 - llmwire.ContextInputFraction) * float64(window))

	resp, err := s.chat(ctx, s.prompt.SystemPrompt(), messages, schemas, llmwire.WithMaxTokens(reserve))
	if err != nil {
		return "", nil, fmt.Errorf("compaction chat: %w", err)
	}

	acc := &compactionUsage{}
	acc.add(resp)

	if len(resp.ToolCalls) > 0 {
		retry, err := s.rejectSummarizerToolCall(ctx, messages, schemas, reserve, resp)
		if err != nil {
			return "", acc, err
		}

		acc.add(retry)
		resp = retry
	}

	summaryText, err := acceptedCheckpointText(resp)
	if err != nil {
		return "", acc, err
	}

	return summaryText, acc, nil
}

// rejectSummarizerToolCall answers a tool-calling summarizer once, in role:
// the call keeps its recorded tool results, so the transcript stays provider-
// valid, and the demand to summarize is restated. One nudge only.
func (s *Session) rejectSummarizerToolCall(
	ctx context.Context,
	messages []llmwire.Message,
	schemas []llmwire.ToolSchema,
	reserve int,
	resp *llmwire.Response,
) (*llmwire.Response, error) {
	replies := make([]llmwire.Message, 0, len(resp.ToolCalls))

	for _, tc := range resp.ToolCalls {
		replies = append(replies, llmwire.Message{
			Role:       llmwire.RoleTool,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    "TOOLS ARE UNAVAILABLE. Do not call any tools. I am waiting for the summary text right now.",
		})
	}

	followUp := append(append([]llmwire.Message{}, messages...), replies...)

	retry, err := s.chat(ctx, s.prompt.SystemPrompt(), followUp, schemas, llmwire.WithMaxTokens(reserve))
	if err != nil {
		return nil, fmt.Errorf("compaction retry after tool call: %w", err)
	}

	return retry, nil
}

// acceptedCheckpointText validates the single accepted shape: one fully
// completed, non-empty text response with no tool calls. Missing headings or a
// short answer are fine; anything else is not a checkpoint.
func acceptedCheckpointText(resp *llmwire.Response) (string, error) {
	if resp == nil {
		return "", errors.New("empty summarizer response")
	}

	if len(resp.ToolCalls) > 0 {
		return "", errors.New("summarizer attempted tool calls")
	}

	switch resp.FinishType {
	case llmwire.FinishStop:
	case llmwire.FinishLength:
		return "", errors.New("summarizer output stopped for length")
	default:
		return "", fmt.Errorf("summarizer finished with %q, not a normal completion", resp.FinishType)
	}

	if strings.TrimSpace(resp.Text) == "" {
		return "", errors.New("summarizer returned no text")
	}

	return strings.TrimSpace(resp.Text), nil
}

// compactionInstructionMessage renders the final role-user instruction:
// the revised checkpoint prompt plus the optional /compact focus.
func compactionInstructionMessage(focus string) llmwire.Message {
	content := sessionprompt.CompactionSummaryPrompt + focus

	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

// activeBackgroundSection preserves producer identities and waiting guidance across compaction.
func (s *Session) activeBackgroundSection(context.Context) string {
	return s.activeBackgroundSnapshot
}

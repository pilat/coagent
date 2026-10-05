package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// toolCallResultItem holds the decided outcome of one scheduled tool call.
// Only items whose outcome is executed or failed carry persisted content;
// suspended and cancelled items stay out of the transcript.
type toolCallResultItem struct {
	index          int
	toolCall       llmwire.ToolCall
	content        string
	untrusted      bool
	images         []llmwire.ImageRef
	directMessages []string
	outcome        tool.Outcome
	// err keeps the raw execution error for logging only.
	err error
}

// batchConflict rejects a call that cannot share an assistant turn with its
// siblings: only sleep is invalid next to task/send_to_subagent — earlier
// stages may run, the sleep fails as its own barrier stage, later stages skip.
func batchConflict(batch []llmwire.ToolCall, call llmwire.ToolCall) error {
	hasTask, hasFollowUp := false, false

	for _, sibling := range batch {
		switch sibling.Name {
		case tool.IDTask:
			hasTask = true
		case tool.IDSendToSubagent:
			hasFollowUp = true
		}
	}

	// A sleep alongside a subagent call cannot wait for it and creates a second,
	// competing suspension protocol.
	if (hasTask || hasFollowUp) && call.Name == tool.IDSleep {
		conflict := tool.IDTask
		if !hasTask {
			conflict = tool.IDSendToSubagent
		}

		return fmt.Errorf(
			"sleep cannot be combined with %s: subagent completion wakes the session automatically",
			conflict,
		)
	}

	return nil
}

func failedItem(index int, tc llmwire.ToolCall, err error, contextWindow int) toolCallResultItem {
	content := fmt.Sprintf("Error: %v", err)
	if tool.IsUntrustedOutputSource(tc.Name) {
		content = wrapUntrustedContent(content, contextWindow)
	}

	return toolCallResultItem{
		index: index, toolCall: tc, content: content,
		untrusted: tool.IsUntrustedOutputSource(tc.Name), outcome: tool.OutcomeFailed, err: err,
	}
}

// executeToolCallsInternal schedules the assistant turn's calls through the
// shared executor and returns decided items in call order without committing
// them to the transcript.
func executeToolCallsInternal(ctx context.Context, agent *Session, toolCalls []llmwire.ToolCall) []toolCallResultItem {
	log := logger.Ctx(ctx).Named("session.toolexec")
	contextWindow := agent.contextWindow()

	results := make([]toolCallResultItem, len(toolCalls))

	// Activation-only rule: an activated mutation must be the only call in its
	// command turn, and no sibling side effect may start. Rejected as a whole
	// before any execution.
	if agent.currentActivation != nil {
		ctx = tool.WithActivationGrant(ctx, *agent.currentActivation)

		if len(toolCalls) > 1 && slices.ContainsFunc(toolCalls, func(call llmwire.ToolCall) bool {
			return call.Name == agent.currentActivation.ToolID
		}) {
			for i, call := range toolCalls {
				inv := failedItem(i, call, fmt.Errorf(
					"%s must be invoked alone in its activated command turn", agent.currentActivation.ToolID,
				), contextWindow)
				results[i] = inv
			}

			return results
		}
	}

	// Resolve once while planning: the same tool instance is classified and
	// executed, so a mid-turn registry swap cannot split the policy from the
	// execution.
	calls := make([]tool.Call, len(toolCalls))
	for i, tc := range toolCalls {
		err := batchConflict(toolCalls, tc)
		if err != nil {
			log.Warn("tool_conflict", zap.String("name", tc.Name), zap.Error(err))
		}

		calls[i] = tool.Call{
			Tool:      agent.registry.Get(tc.Name),
			Name:      tc.Name,
			ID:        tc.ID,
			Arguments: tc.Arguments,
			Err:       err,
		}
	}

	report := tool.Schedule(ctx, calls)

	// One summary per scheduling invocation on the native path.
	log.Info("tool_schedule",
		zap.Int("calls", report.Summary.Calls),
		zap.Int("stages", report.Summary.Stages),
		zap.Int("max_parallel", report.Summary.MaxParallel),
		zap.Int("executed", report.Summary.Executed),
		zap.Int("skipped", report.Summary.Skipped),
		zap.Int("failed", report.Summary.Failed),
		zap.Int("suspended", report.Summary.Suspended),
		zap.Int64("duration_ms", report.Summary.DurationMS),
	)

	return mapExecutorReport(agent, log, toolCalls, report)
}

// mapExecutorReport folds the executor's ordered report into decided items,
// preserving assistant call order. Outcome slots are never left zero: a zero
// outcome reads as executed and would commit an empty result row.
func mapExecutorReport(
	agent *Session,
	log *zap.Logger,
	toolCalls []llmwire.ToolCall,
	report tool.Report,
) []toolCallResultItem {
	results := make([]toolCallResultItem, len(toolCalls))

	for i, r := range report.Results {
		tc := toolCalls[i]

		switch r.Outcome {
		case tool.OutcomeCancelled:
			// Cancellation propagates: stop/kill settles unresolved calls and
			// the cancelled context answers the turn. No fabricated errors.
			// The slot still carries its outcome: a zero value reads as executed.
			results[i].outcome = tool.OutcomeCancelled
			results[i].toolCall = tc
		case tool.OutcomeSuspended:
			// Owned pending call: the real result is injected on resume.
			log.Info("tool_suspended", zap.String("name", tc.Name))

			agent.suspended = true
			results[i].outcome = tool.OutcomeSuspended
			results[i].toolCall = tc
		case tool.OutcomeSkipped:
			results[i] = toolCallResultItem{
				index:    i,
				toolCall: tc,
				content:  tool.ErrSkipped.Error(),
				outcome:  tool.OutcomeSkipped,
			}
		case tool.OutcomeExecuted, tool.OutcomeFailed:
			if r.Result == nil {
				results[i] = failedItem(i, tc, r.Err, agent.contextWindow())
				continue
			}

			results[i] = toolCallResultItem{
				index: i, toolCall: tc, content: formatToolResult(r.Result, agent.contextWindow()),
				untrusted: r.Result.Untrusted, images: r.Result.Images,
				directMessages: r.Result.DirectMessages, outcome: r.Outcome,
			}
			if r.Outcome == tool.OutcomeFailed {
				log.Warn("tool_typed_failure", zap.String("name", tc.Name))
			} else {
				log.Info("result", zap.String("name", tc.Name), zap.Int("size", len(r.Result.Output)))
			}
		}
	}

	return results
}

// executeToolCalls orchestrates tool execution with loop detection.
func executeToolCalls(ctx context.Context, agent *Session, toolCalls []llmwire.ToolCall) error {
	log := logger.Ctx(ctx).Named("session.toolexec")

	action := agent.loopDetector.check()
	if action == actionBlock || action == actionForceTextOnly {
		log.Warn("tool_calls_blocked", zap.Int("action", int(action)), zap.Int("count", len(toolCalls)))

		results := make([]*transcript.Message, 0, len(toolCalls))
		for _, tc := range toolCalls {
			results = append(
				results,
				&transcript.Message{
					Role:       llmwire.RoleTool,
					Content:    loopBlockMessage,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					ToolError:  true,
				},
			)
		}

		c := agent.newCommit()
		c.ToolResults = results
		_, err := agent.commit(ctx, c)

		return err
	}

	results := executeToolCallsInternal(ctx, agent, toolCalls)

	// The diversity window records only callbacks that ran to a terminal
	// outcome; suspended, skipped and cancelled calls never enter it.
	records := make([]toolRecord, 0, len(results))
	for _, r := range results {
		if r.outcome != tool.OutcomeExecuted && r.outcome != tool.OutcomeFailed {
			continue
		}

		records = append(records, toolRecord{
			name:       r.toolCall.Name,
			argsHash:   fingerprintArgs(r.toolCall.Arguments),
			resultHash: fingerprintResult(r.content, r.images...),
			failed:     r.outcome == tool.OutcomeFailed,
		})
	}

	agent.loopDetector.record(records)

	return recordToolResults(ctx, agent, results, agent.loopDetector.check())
}

// recordToolResults commits the decided set before activation/progress events.
// Only the final persisted result receives a loop warning.
func recordToolResults(
	ctx context.Context,
	agent *Session,
	results []toolCallResultItem,
	postAction loopAction,
) error {
	// The warning fronts only the turn's last persisted executed/failed result,
	// so its index must be fixed before the commit loop walks the rows.
	warnIdx := -1

	for i, r := range results {
		if r.outcome == tool.OutcomeExecuted || r.outcome == tool.OutcomeFailed {
			warnIdx = i
		}
	}

	c := agent.newCommit()

	for i, r := range results {
		if r.outcome == tool.OutcomeSuspended || r.outcome == tool.OutcomeCancelled {
			continue
		}

		direct := make([]string, len(r.directMessages))
		for j, message := range r.directMessages {
			direct[j] = logger.Redact(message)
		}

		// One row must fit the store's direct-output budget or the whole
		// turn fails to commit; the batch fallback aggregates several
		// children's outputs into one row, so trim here for both paths.
		direct = capDirectOutput(direct)

		content := r.content
		if r.untrusted {
			content = identifyUntrustedContent(content)
		}

		if i == warnIdx {
			content = prependLoopWarning(ctx, agent, postAction, r.toolCall.Name, content)
		}

		message, err := storedMessage(
			&llmwire.Message{
				Role:       llmwire.RoleTool,
				Content:    content,
				ToolCallID: r.toolCall.ID,
				ToolName:   r.toolCall.Name,
				ToolError:  r.outcome == tool.OutcomeFailed || r.outcome == tool.OutcomeSkipped,
				Images:     r.images,
			},
		)
		if err != nil {
			return err
		}

		c.ToolResults = append(c.ToolResults, message)
		for j, text := range direct {
			c.Outputs = append(
				c.Outputs,
				sessionstore.Output{
					Type:        sessionstore.OutputMessagePersistent,
					Content:     text,
					Key:         fmt.Sprintf("tool:%s:direct:%d", r.toolCall.ID, j),
					PersistOnly: true,
					MessageRef:  -1,
				},
			)
		}
	}

	return commitToolState(ctx, agent, c, results)
}

func commitToolState(ctx context.Context, agent *Session, c sessionstore.Commit, results []toolCallResultItem) error {
	data, err := json.Marshal(agent.prompt.Todos.List())
	if err != nil {
		return fmt.Errorf("record tool results: %w", err)
	}

	raw := json.RawMessage(data)

	c.State.TodoItems = &raw
	if _, err := agent.commit(ctx, c); err != nil {
		return err
	}

	agent.stamper.Touch()
	// All rows are durable; user-facing semantics fire only now.
	for _, r := range results {
		if r.outcome != tool.OutcomeExecuted {
			continue
		}

		if activated := consumeActivation(agent, r); activated {
			if err := publishProgressSnapshot(ctx, agent); err != nil {
				return err
			}
		}
	}

	return nil
}

// consumeActivation clears the current activation grant when its owning tool
// just executed and reports whether the progress snapshot must republish.
func consumeActivation(agent *Session, r toolCallResultItem) bool {
	activatedDirect := len(r.directMessages) > 0 && agent.currentActivation != nil &&
		r.toolCall.Name == agent.currentActivation.ToolID

	if activatedDirect {
		agent.currentActivation = nil
	}

	return r.toolCall.Name == "todowrite" || activatedDirect
}

// publishProgressSnapshot enqueues the TODO progress snapshot through the
// boundary and notifies through the loop's channel, superseding tolerated.
func publishProgressSnapshot(_ context.Context, agent *Session) error {
	agent.emit(sessionevent.Notification{Type: sessionevent.NotifyProgressChanged})
	return nil
}

func (s *Session) toolStep(ctx context.Context, calls []llmwire.ToolCall) error {
	if narratedToolCalls(s.ms.getMessages(), calls) {
		s.emit(sessionevent.Notification{Type: sessionevent.NotifyProgressChanged})
	}

	err := executeToolCalls(ctx, s, calls)
	if s.suspended {
		if s.stagedCalls == nil {
			s.stagedCalls = map[string]string{}
		}

		for _, call := range calls {
			if tool.IsExternalCall(call.Name) && unresolvedToolCalls(s.ms.getMessages())[call.ID] == call.Name {
				s.stagedCalls[call.ID] = call.Name
			}
		}
	}

	return err
}

func narratedToolCalls(messages []llmwire.Message, calls []llmwire.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}

	for _, message := range slices.Backward(messages) {
		for _, call := range message.ToolCalls {
			if call.ID == calls[0].ID {
				return strings.TrimSpace(message.Content) != ""
			}
		}
	}

	return false
}

// capDirectOutput trims one result row's direct output to the store's per-row
// budget: the batch fallback aggregates several children's outputs into one
// row, and an over-budget row would fail the whole turn's commit.
func capDirectOutput(direct []string) []string {
	validCount := 0
	validBytes := 0

	for _, message := range direct {
		if message == "" || len(message) > sessionstore.MaxDirectMessageBytes {
			continue
		}

		validCount++
		validBytes += len(message)
	}

	// Store-valid input within every budget passes through untouched;
	// anything else goes through capping, which also strips invalid entries.
	if validCount == len(direct) &&
		validCount <= sessionstore.MaxDirectMessages &&
		validBytes <= sessionstore.MaxDirectTotalBytes {
		return direct
	}

	// Something will be omitted, so the notice occupies the last slot.
	limit := sessionstore.MaxDirectMessages - 1

	capped := make([]string, 0, limit+1)
	omitted := len(direct)
	budget := sessionstore.MaxDirectTotalBytes

	for _, message := range direct {
		if len(capped) == limit {
			break
		}

		// Empty and oversized messages are store-rejected individually; skip
		// them but keep admitting smaller later ones.
		if message == "" || len(message) > sessionstore.MaxDirectMessageBytes || len(message) > budget {
			continue
		}

		capped = append(capped, message)
		budget -= len(message)
		omitted--
	}

	return append(capped, fmt.Sprintf("[direct output truncated: %d messages omitted]", omitted))
}

// prependLoopWarning returns content fronted by the detector's warning, or
// unchanged when postAction asks for none.
func prependLoopWarning(ctx context.Context, agent *Session, postAction loopAction, toolName, content string) string {
	log := logger.Ctx(ctx).Named("session.toolexec")

	switch postAction {
	case actionWarn:
		uniqueOutcomes := countUniqueOutcomes(agent.loopDetector.window)
		windowLen := len(agent.loopDetector.window)
		diversityPct := 0

		if windowLen > 0 {
			diversityPct = uniqueOutcomes * 100 / windowLen
		}

		log.Warn("loop_warning_prepended", zap.Int("diversity_pct", diversityPct))

		return fmt.Sprintf(loopWarningTemplate, diversityPct, windowLen, uniqueOutcomes) + "\n\n" + content
	case actionWarnFailure:
		streak := agent.loopDetector.consecutiveFailureStreak()

		log.Warn("loop_failure_warning_prepended", zap.String("tool", toolName), zap.Int("streak", streak))

		return fmt.Sprintf(loopFailureWarningTemplate, toolName, streak) + "\n\n" + content
	case actionNone, actionBlock, actionForceTextOnly:
		// no warning to prepend for these outcomes
	}

	return content
}

// countUniqueOutcomes returns the number of unique (name, resultHash) pairs in the window.
func countUniqueOutcomes(window []toolRecord) int {
	type key struct {
		name       string
		resultHash uint64
	}

	seen := make(map[key]struct{}, len(window))
	for _, r := range window {
		seen[key{r.name, r.resultHash}] = struct{}{}
	}

	return len(seen)
}

// External titles and notices share the payload budget; local results retain
// their historical output-only truncation.
func formatToolResult(result *tool.Result, contextWindow int) string {
	if result.Untrusted {
		var sb strings.Builder

		appendResultTitle(&sb, result.Title)
		sb.WriteString(result.Output)
		appendTruncationNotice(&sb, result)

		return wrapUntrustedContent(sb.String(), contextWindow)
	}

	var sb strings.Builder

	appendResultTitle(&sb, result.Title)
	sb.WriteString(truncateHeadTail(result.Output, tool.DynamicToolResultBudgetForWindow(contextWindow)))
	appendTruncationNotice(&sb, result)

	return sb.String()
}

func appendResultTitle(sb *strings.Builder, title string) {
	if title != "" {
		fmt.Fprintf(sb, "[%s]\n", title)
	}
}

func appendTruncationNotice(sb *strings.Builder, result *tool.Result) {
	// Tools self-report truncation in metadata; the notice counts the raw bytes.
	if result.Metadata != nil {
		if t, ok := result.Metadata["truncated"].(bool); ok && t {
			fmt.Fprintf(sb, "\n(output truncated: %d bytes total)", len(result.Output))
		}
	}
}

// Truncate before escaping so a cut cannot recreate a boundary token.
func wrapUntrustedContent(payload string, contextWindow int) string {
	begin := tool.UntrustedContentBegin
	end := tool.UntrustedContentEnd

	payload = truncateHeadTail(payload, tool.DynamicToolResultBudgetForWindow(contextWindow))

	for _, marker := range []string{begin, end} {
		prefix := strings.TrimSuffix(marker, ">>>")
		payload = strings.ReplaceAll(payload, prefix, prefix+"_ESCAPED")
	}

	return begin + "\n" + payload + "\n" + end
}

// Add randomness after loop fingerprinting and before persistence, so repeated
// results still compare equal and replay never regenerates a boundary ID.
func identifyUntrustedContent(content string) string {
	var entropy [8]byte
	_, _ = rand.Read(entropy[:])

	markerID := hex.EncodeToString(entropy[:])
	payload := strings.TrimSuffix(strings.TrimPrefix(content, tool.UntrustedContentBegin+"\n"),
		"\n"+tool.UntrustedContentEnd)

	return fmt.Sprintf("%s id=%q>>>\n%s\n%s id=%q>>>",
		strings.TrimSuffix(tool.UntrustedContentBegin, ">>>"), markerID, payload,
		strings.TrimSuffix(tool.UntrustedContentEnd, ">>>"), markerID)
}

// truncateHeadTail truncates s to maxRunes using a head+tail strategy,
// splitting 70% head / 30% tail by default. If the tail contains
// error/stack-trace patterns, the split is adjusted to 50/50 to preserve
// more tail context.
// Cut points are snapped to the nearest newline boundary when possible.
// A middle marker showing omitted size is counted inside the limit.
// Returns s unchanged if under maxRunes.
func truncateHeadTail(s string, maxRunes int) string {
	const defaultHeadRatio = 0.7

	headRatio := defaultHeadRatio

	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	marker := fmt.Sprintf("\n... (omitted %d chars) ...\n", len(runes)-maxRunes)
	markerRunes := []rune(marker)

	available := maxRunes - len(markerRunes)
	if available <= 0 {
		// Degenerate case: marker alone exceeds limit
		return string(runes[:maxRunes])
	}

	// Adjust ratio if tail contains error/stack-trace content
	if available > 200 && hasImportantTail(s) {
		headRatio = 0.5
	}

	headSize := int(float64(available) * headRatio)
	tailSize := available - headSize

	// Snap to newline boundaries for readability
	maxDrift := min(200, available/5)
	headSize = snapToNewline(runes, headSize, -1, maxDrift)
	tailStart := len(runes) - tailSize
	tailStart = snapToNewline(runes, tailStart, +1, maxDrift)

	// Guard: ensure tailStart stays within bounds and tail is non-empty
	if tailStart >= len(runes) {
		tailStart = len(runes) - tailSize // revert to exact
	}

	tailSize = len(runes) - tailStart

	// Ensure we don't exceed budget after snapping
	if headSize+tailSize > available {
		// Revert to exact cuts
		headSize = int(float64(available) * headRatio)
		tailSize = available - headSize
		tailStart = len(runes) - tailSize
	}

	head := runes[:headSize]
	tail := runes[tailStart:]

	return string(head) + marker + string(tail)
}

// hasImportantTail checks whether the tail of a string contains error or
// diagnostic content worth preserving. Inspects the last ~2000 runes.
// Uses specific multi-word patterns to avoid false positives on normal output.
func hasImportantTail(s string) bool {
	tailSample := s

	if utf8.RuneCountInString(s) > 2000 {
		runes := []rune(s)
		tailSample = string(runes[len(runes)-2000:])
	}

	lower := strings.ToLower(tailSample)

	// Error/diagnostic patterns — multi-word to reduce false positives
	for _, pattern := range []string{
		"error:", "exception:", "fatal:", "fatal error",
		"traceback (most recent", "panic:", "stack trace",
		"errno", "exit code", "exit status",
		"--- fail:", "build failed",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}

	// JSON closing brace/bracket at end of content
	trimmed := strings.TrimRight(tailSample, " \t\n\r")
	if strings.HasSuffix(trimmed, "}") || strings.HasSuffix(trimmed, "]") {
		return true
	}

	return false
}

// snapToNewline adjusts pos to the nearest newline boundary.
// direction -1 searches backward (for head cuts), +1 searches forward (for tail starts).
// maxDrift limits how far to search. Returns original pos if no newline found within range.
func snapToNewline(runes []rune, pos, direction, maxDrift int) int {
	if pos <= 0 || pos >= len(runes) || maxDrift <= 0 {
		return pos
	}

	if direction == -1 {
		// Search backward for '\n', return the position after it (start of next line)
		for i := pos; i >= pos-maxDrift && i >= 0; i-- {
			if runes[i] == '\n' {
				return i + 1
			}
		}

		return pos
	}

	// Search forward for '\n', return the position after it
	limit := min(pos+maxDrift, len(runes)-1)
	for i := pos; i <= limit; i++ {
		if runes[i] == '\n' {
			return i + 1
		}
	}

	return pos
}

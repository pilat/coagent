package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/toolexec"
)

// toolCallResultItem holds the decided outcome of one scheduled tool call.
// Only items whose outcome is executed or failed carry persisted content;
// suspended and cancelled items stay out of the transcript.
type toolCallResultItem struct {
	index          int
	toolCall       llmwire.ToolCall
	content        string
	images         []llmwire.ImageRef
	directMessages []string
	outcome        toolexec.Outcome
	// err keeps the raw execution error for logging only.
	err error
}

// plannedToolCall pairs one assistant-turn call with the registry instance
// resolved for it, so classification and execution observe the same registry
// state even if the registry changes mid-turn.
type plannedToolCall struct {
	call llmwire.ToolCall
	tool tool.Tool
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

// failedItem builds an item for a call that ran to a typed failure.
func failedItem(index int, tc llmwire.ToolCall, err error) toolexec.Invocation[toolCallResultItem] {
	return toolexec.Invocation[toolCallResultItem]{
		Outcome: toolexec.OutcomeFailed,
		Err:     err,
		Result: toolCallResultItem{
			index:    index,
			toolCall: tc,
			content:  fmt.Sprintf("Error: %v", err),
			outcome:  toolexec.OutcomeFailed,
			err:      err,
		},
	}
}

// executeToolCallsInternal schedules the assistant turn's calls through the
// shared executor and returns decided items in call order without committing
// them to the transcript.
func executeToolCallsInternal(ctx context.Context, agent *svc, toolCalls []llmwire.ToolCall) []toolCallResultItem {
	log := logger.Ctx(ctx).Named("session.toolexec")

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
				))
				results[i] = inv.Result
			}

			return results
		}
	}

	// Resolve once while planning: the same tool instance is classified and
	// executed, so a mid-turn registry swap cannot split the policy from the
	// execution.
	calls := make([]toolexec.Call[plannedToolCall], len(toolCalls))
	for i, tc := range toolCalls {
		tl := agent.registry.Get(tc.Name)

		calls[i] = toolexec.Call[plannedToolCall]{
			Call: plannedToolCall{
				call: tc,
				tool: tl,
			},
			ParallelSafe: tl != nil && tl.ParallelSafe(),
		}
	}

	exec := func(callCtx context.Context, idx int, pc plannedToolCall) toolexec.Invocation[toolCallResultItem] {
		// Only sleep may be invalid next to task/send_to_subagent; it fails as
		// its own singleton barrier without its earlier siblings paying for it.
		if err := batchConflict(toolCalls, pc.call); err != nil {
			log.Warn("tool_conflict", zap.String("name", pc.call.Name), zap.Error(err))

			return failedItem(idx, pc.call, err)
		}

		if pc.tool == nil {
			return failedItem(idx, pc.call, fmt.Errorf("unknown tool: %s", pc.call.Name))
		}

		// Bind this call's id to the callback context so tools (e.g. task) can
		// resolve the spawning tool_call id. The session loop is the single
		// chokepoint both the initial turn and resume re-execution funnel
		// through (Appendix G2).
		callCtx = tool.WithCallID(callCtx, pc.call.ID)

		log.Info("tool_call", zap.String("name", pc.call.Name))

		return executeOneTool(callCtx, pc.tool, pc.call, idx, agent.contextWindow(), log)
	}

	report := toolexec.Schedule(ctx, calls, exec)

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
	agent *svc,
	log *zap.Logger,
	toolCalls []llmwire.ToolCall,
	report toolexec.Report[toolCallResultItem],
) []toolCallResultItem {
	results := make([]toolCallResultItem, len(toolCalls))

	for i, r := range report.Results {
		tc := toolCalls[i]

		switch r.Outcome {
		case toolexec.OutcomeCancelled:
			// Cancellation propagates: stop/kill settles unresolved calls and
			// the cancelled context answers the turn. No fabricated errors.
			// The slot still carries its outcome: a zero value reads as executed.
			results[i].outcome = toolexec.OutcomeCancelled
			results[i].toolCall = tc
		case toolexec.OutcomeSuspended:
			// Owned pending call: the real result is injected on resume.
			log.Info("tool_suspended", zap.String("name", tc.Name))

			agent.suspended = true
			results[i].outcome = toolexec.OutcomeSuspended
			results[i].toolCall = tc
		case toolexec.OutcomeSkipped:
			results[i] = toolCallResultItem{
				index:    i,
				toolCall: tc,
				content:  toolexec.ErrSkipped.Error(),
				outcome:  toolexec.OutcomeSkipped,
			}
		case toolexec.OutcomeExecuted, toolexec.OutcomeFailed:
			results[i] = r.Result
			results[i].index = i
			results[i].toolCall = tc
		}
	}

	return results
}

// executeOneTool runs one resolved tool and decides the invocation's outcome.
// A Go error, recovered panic, nil result or Result.IsError marks a failure;
// ErrSuspend is an owned pending call rather than a failure.
func executeOneTool(
	ctx context.Context,
	tl tool.Tool,
	tc llmwire.ToolCall,
	index int,
	contextWindow int,
	log *zap.Logger,
) toolexec.Invocation[toolCallResultItem] {
	// A panicking frame cannot return a value; recover into an outer slot and
	// classify after the closure unwinds.
	var outcome struct {
		inv   toolexec.Invocation[toolCallResultItem]
		panic any
	}

	func() {
		defer func() {
			outcome.panic = recover()
		}()

		outcome.inv = runResolvedTool(ctx, tl, tc, index, contextWindow, log)
	}()

	if outcome.panic != nil {
		return failedItem(index, tc, fmt.Errorf("panic in tool %s: %v", tc.Name, outcome.panic))
	}

	return outcome.inv
}

// runResolvedTool is executeOneTool's panic-free body: classify the tool's
// execution result into a typed invocation.
func runResolvedTool(
	ctx context.Context,
	tl tool.Tool,
	tc llmwire.ToolCall,
	index int,
	contextWindow int,
	log *zap.Logger,
) toolexec.Invocation[toolCallResultItem] {
	result, err := tl.Execute(ctx, tc.Arguments)
	if err != nil {
		if errors.Is(err, tool.ErrSuspend) {
			return toolexec.Invocation[toolCallResultItem]{
				Outcome: toolexec.OutcomeSuspended,
				Err:     fmt.Errorf("execute tool %s: %w", tc.Name, err),
			}
		}

		return failedItem(index, tc, fmt.Errorf("execute tool %s: %w", tc.Name, err))
	}

	if result == nil {
		return failedItem(index, tc, fmt.Errorf("execute tool %s: tool returned nil result", tc.Name))
	}

	item := toolCallResultItem{
		index:          index,
		toolCall:       tc,
		content:        formatToolResult(result, contextWindow),
		images:         result.Images,
		directMessages: result.DirectMessages,
		outcome:        toolexec.OutcomeExecuted,
	}

	if result.IsError {
		item.outcome = toolexec.OutcomeFailed
	}

	if item.outcome == toolexec.OutcomeFailed {
		log.Warn("tool_typed_failure", zap.String("name", tc.Name))
	} else {
		log.Info("result", zap.String("name", tc.Name), zap.Int("size", len(result.Output)))
	}

	return toolexec.Invocation[toolCallResultItem]{
		Outcome: item.outcome,
		Result:  item,
	}
}

// executeToolCalls orchestrates tool execution with loop detection.
func executeToolCalls(ctx context.Context, agent *svc, toolCalls []llmwire.ToolCall) error {
	log := logger.Ctx(ctx).Named("session.toolexec")

	action := agent.loopDetector.check()
	if action == actionBlock || action == actionForceTextOnly {
		log.Warn("tool_calls_blocked", zap.Int("action", int(action)), zap.Int("count", len(toolCalls)))

		for _, tc := range toolCalls {
			if err := agent.ms.addToolResult(ctx, tc.ID, tc.Name, loopBlockMessage); err != nil {
				return err
			}
		}

		return nil
	}

	results := executeToolCallsInternal(ctx, agent, toolCalls)

	// The diversity window records only callbacks that ran to a terminal
	// outcome; suspended, skipped and cancelled calls never enter it.
	records := make([]toolRecord, 0, len(results))
	for _, r := range results {
		if r.outcome != toolexec.OutcomeExecuted && r.outcome != toolexec.OutcomeFailed {
			continue
		}

		records = append(records, toolRecord{
			name:       r.toolCall.Name,
			argsHash:   fingerprintArgs(r.toolCall.Arguments),
			resultHash: fingerprintResult(r.content),
			failed:     r.outcome == toolexec.OutcomeFailed,
		})
	}

	agent.loopDetector.record(records)

	return recordToolResults(ctx, agent, results, agent.loopDetector.check())
}

// recordToolResults commits the decided non-pending set in one transaction,
// then runs activation consumption and progress hooks. The loop-detector
// warning fronts the last persisted executed or failed result, or none is
// emitted when the turn produced no such result.
func recordToolResults(
	ctx context.Context,
	agent *svc,
	results []toolCallResultItem,
	postAction loopAction,
) error {
	// The warning fronts only the turn's last persisted executed/failed result,
	// so its index must be fixed before the commit loop walks the rows.
	warnIdx := -1

	for i, r := range results {
		if r.outcome == toolexec.OutcomeExecuted || r.outcome == toolexec.OutcomeFailed {
			warnIdx = i
		}
	}

	commits := make([]toolResultCommit, 0, len(results))

	for i, r := range results {
		if r.outcome == toolexec.OutcomeSuspended || r.outcome == toolexec.OutcomeCancelled {
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
		if i == warnIdx {
			content = prependLoopWarning(ctx, agent, postAction, r.toolCall.Name, content)
		}

		commits = append(commits, toolResultCommit{
			message: llmwire.Message{
				Role:       llmwire.RoleTool,
				Content:    content,
				ToolCallID: r.toolCall.ID,
				ToolName:   r.toolCall.Name,
				ToolError:  r.outcome == toolexec.OutcomeFailed || r.outcome == toolexec.OutcomeSkipped,
				Images:     r.images,
			},
			direct: direct,
		})
	}

	if err := agent.ms.commitToolResults(ctx, commits); err != nil {
		return err
	}

	// All rows are durable; user-facing semantics fire only now.
	for _, r := range results {
		if r.outcome != toolexec.OutcomeExecuted {
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
func consumeActivation(agent *svc, r toolCallResultItem) bool {
	activatedDirect := len(r.directMessages) > 0 && agent.currentActivation != nil &&
		r.toolCall.Name == agent.currentActivation.ToolID

	if activatedDirect {
		agent.currentActivation = nil
	}

	return r.toolCall.Name == "todowrite" || activatedDirect
}

// publishProgressSnapshot enqueues the TODO progress snapshot through the
// boundary and notifies through the loop's channel, superseding tolerated.
func publishProgressSnapshot(ctx context.Context, agent *svc) error {
	provider, ok := agent.boundary.(progressChangeBoundary)
	if !ok {
		return nil
	}

	message, published, progressErr := provider.ProgressChange(ctx)
	if progressErr != nil && !errors.Is(progressErr, sessionstore.ErrProgressSuperseded) {
		return fmt.Errorf("enqueue TODO progress snapshot: %w", progressErr)
	}

	if published && agent.loopOpts.Notify != nil {
		_ = agent.loopOpts.Notify(ctx, message)
	}

	return nil
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
func prependLoopWarning(ctx context.Context, agent *svc, postAction loopAction, toolName, content string) string {
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

func formatToolResult(result *tool.Result, contextWindow int) string {
	var sb strings.Builder

	if result.Title != "" {
		fmt.Fprintf(&sb, "[%s]\n", result.Title)
	}

	output := truncateHeadTail(result.Output, tool.DynamicToolResultBudgetForWindow(contextWindow))
	sb.WriteString(output)

	// Add truncation notice from tools that self-report truncation
	if result.Metadata != nil {
		if t, ok := result.Metadata["truncated"].(bool); ok && t {
			fmt.Fprintf(&sb, "\n(output truncated: %d bytes total)", len(result.Output))
		}
	}

	return sb.String()
}

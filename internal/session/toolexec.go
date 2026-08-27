package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

// toolCallResultItem holds the result of a single tool call execution.
type toolCallResultItem struct {
	index    int
	toolCall llmwire.ToolCall
	result   string
	images   []llmwire.ImageRef
	err      error
}

// batchConflict rejects a call that cannot share an assistant turn with its
// siblings: calls in one response run concurrently and cannot order each other.
func batchConflict(batch []llmwire.ToolCall, call llmwire.ToolCall) error {
	hasTask, hasFollowUp, suspending := false, false, ""

	for _, sibling := range batch {
		switch sibling.Name {
		case tool.IDTask:
			hasTask = true
		case tool.IDSendToSubagent:
			hasFollowUp = true
		}

		if tool.IsExternalCall(sibling.Name) || sibling.Name == tool.IDSendToSubagent {
			suspending = sibling.Name
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

	// The flag a compaction request raises dies with the svc a suspend rebuilds;
	// refuse the combination rather than lose the request silently.
	if suspending != "" && call.Name == tool.IDCompactContext {
		return fmt.Errorf(
			"cannot compact in the same turn as %s — call compact_context again after it completes",
			suspending,
		)
	}

	return nil
}

// executeToolCallsInternal runs tool calls in parallel and returns the results
// without adding them to messages. This allows the caller to inspect and modify
// results before committing them.
func executeToolCallsInternal(ctx context.Context, agent *svc, toolCalls []llmwire.ToolCall) []toolCallResultItem {
	log := logger.Ctx(ctx).Named("session.toolexec")

	results := make([]toolCallResultItem, len(toolCalls))

	var wg sync.WaitGroup

	for i, tc := range toolCalls {
		if err := batchConflict(toolCalls, tc); err != nil {
			results[i] = toolCallResultItem{index: i, toolCall: tc, err: err}
			continue
		}

		wg.Add(1)

		go func(idx int, tc llmwire.ToolCall) {
			defer func() {
				if r := recover(); r != nil {
					results[idx] = toolCallResultItem{
						index:    idx,
						toolCall: tc,
						err:      fmt.Errorf("panic in tool %s: %v", tc.Name, r),
					}
				}

				wg.Done()
			}()

			log.Info("tool_call", zap.String("name", tc.Name))

			content, images, err := executeToolCall(ctx, agent.registry, tc, agent.contextWindow())
			results[idx] = toolCallResultItem{
				index:    idx,
				toolCall: tc,
				result:   content,
				images:   images,
				err:      err,
			}
		}(i, tc)
	}

	wg.Wait()

	return results
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

	records := make([]toolRecord, 0, len(results))

	for _, r := range results {
		resultStr := r.result
		if r.err != nil {
			resultStr = r.err.Error()
		}

		records = append(records, toolRecord{
			name:       r.toolCall.Name,
			argsHash:   fingerprintArgs(r.toolCall.Arguments),
			resultHash: fingerprintResult(resultStr),
			failed:     r.err != nil,
		})
	}

	// Record and check diversity
	agent.loopDetector.record(records)

	return recordToolResults(ctx, agent, results, agent.loopDetector.check())
}

// recordToolResults appends every result in call order, prefixing the last one
// with the loop-detector warning when postAction asks for it.
func recordToolResults(
	ctx context.Context,
	agent *svc,
	results []toolCallResultItem,
	postAction loopAction,
) error {
	log := logger.Ctx(ctx).Named("session.toolexec")

	for i, r := range results {
		// ErrSuspend: tool requested session suspend — skip recording result.
		if errors.Is(r.err, tool.ErrSuspend) {
			log.Info("tool_suspended", zap.String("name", r.toolCall.Name))

			agent.suspended = true

			continue
		}

		resultContent := r.result
		images := r.images

		if r.err != nil {
			log.Warn("exec_failed", zap.String("name", r.toolCall.Name), zap.Error(r.err))
			resultContent = fmt.Sprintf("Error: %v", r.err)
			// An error stub never claims pixels (D7).
			images = nil
		} else {
			log.Info("result", zap.String("name", r.toolCall.Name), zap.Int("size", len(r.result)))
		}

		// Prepend a warning to the last tool result if the detector says so.
		if i == len(results)-1 {
			resultContent = prependLoopWarning(ctx, agent, postAction, r.toolCall.Name, resultContent)
		}

		if err := agent.ms.addToolResultRef(ctx, r.toolCall.ID, r.toolCall.Name, resultContent, images); err != nil {
			return err
		}
	}

	return nil
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

func executeToolCall(
	ctx context.Context,
	registry tool.Registry,
	call llmwire.ToolCall,
	contextWindow int,
) (string, []llmwire.ImageRef, error) {
	t := registry.Get(call.Name)
	if t == nil {
		return "", nil, fmt.Errorf("unknown tool: %s", call.Name)
	}

	// Bind this call's id to ctx per-goroutine so tools (e.g. task) can resolve
	// the spawning tool_call id. Set here — the single chokepoint both the initial
	// batch and resume re-execution funnel through (Appendix G2).
	ctx = tool.WithCallID(ctx, call.ID)

	result, err := t.Execute(ctx, call.Arguments)
	if err != nil {
		return "", nil, fmt.Errorf("execute tool %s: %w", call.Name, err)
	}

	if result == nil {
		return "", nil, fmt.Errorf("execute tool %s: tool returned nil result", call.Name)
	}

	return formatToolResult(result, contextWindow), result.Images, nil
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

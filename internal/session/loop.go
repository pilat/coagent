package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const (
	emptyResponseWarnThreshold  = 3
	emptyResponseBreakThreshold = 6
	compactionKeepRecent        = 6

	// compactionAttemptCap is how many consecutive automatic compactions may fail
	// to relieve the pressure before the automatic path stops trying.
	compactionAttemptCap = 3
)

// hardIterationCeiling caps agents declared "unlimited" (MaxIterations == 0): a
// safety net for a loop-detector blind spot, not a normal terminal — real runs
// end far below it.
const hardIterationCeiling = 1000

//nolint:gosec // prompt text shown to the model, not a credential
const loopWarningTemplate = `[LOOP WARNING: Low action diversity (%d%%). Your recent %d tool calls produced only %d unique outcomes.

REQUIRED: Before your next tool call, explain in text WHY your current approach is not working and WHAT specifically you will change. Do not repeat the same strategy.]`

const loopBlockMessage = `[BLOCKED: Tool execution blocked — you were warned about repetitive behavior but continued the same pattern. You MUST respond with text explaining your situation. No tool calls will be executed until you demonstrate a new approach.]`

const loopFailureWarningTemplate = `[LOOP WARNING: The %s tool has returned the same error %d times in a row. Repeating the identical call will not help — fix the arguments or change your approach, or stop and explain the problem in text.]`

type loopResult struct {
	FinalResponse string
	ErrorNotice   string
	Iterations    int
	Error         error
	Suspended     bool // true when a tool (e.g., sleep) requested session suspend
}

type iterationCallback func(iteration int, response *llmwire.Response, toolCalls []llmwire.ToolCall) error

// assistantState describes the state of the last assistant message for resume handling.
type assistantState struct {
	HasPendingTools bool               // assistant has ToolCalls without matching tool results
	PendingTools    []llmwire.ToolCall // the tool calls that need execution
	HasText         bool               // assistant has non-empty text
	Text            string             // the text content
}

// loopOptions holds runtime dependencies passed to Run/RunWithProgress.
// These are per-execution context — main sessions set channels and hooks,
// subagents leave them nil.
type loopOptions struct {
	Notify    func(ctx context.Context, message string) error // callback to deliver messages to the human
	Heartbeat func(ctx context.Context)                       // fire-and-forget activity signal; nil for subagents
}

// loopRunner holds per-run state for a single runLoop invocation.
type loopRunner struct {
	agent              *svc
	opts               loopOptions
	cb                 iterationCallback
	result             *loopResult
	log                *zap.Logger
	maxIter            int
	emptyCount         int
	lastResp           *llmwire.Response
	handledControl     bool
	compactionFailures int
	autoCompactionOff  bool
}

func runLoop(ctx context.Context, agent *svc, opts loopOptions, callback iterationCallback) (*loopResult, error) {
	r := &loopRunner{
		agent:   agent,
		opts:    opts,
		cb:      callback,
		result:  &loopResult{},
		log:     logger.Ctx(ctx).Named("session.loop"),
		maxIter: agent.maxIterations,
	}

	if r.maxIter == 0 {
		r.maxIter = hardIterationCeiling
	}

	// A deferral episode is scoped to the call that caused it; with nothing out
	// with the world, an announcement carried in from an earlier one is stale.
	if !agent.HasPendingExternalCall() {
		agent.compactionDeferAnnounced = false
	}

	hb := newHeartbeatTicker(opts.Heartbeat)
	defer hb.stop()

	hb.start(ctx)

	for r.result.Iterations < r.maxIter {
		select {
		case <-ctx.Done():
			r.result.Error = ctx.Err()
			return r.result, ctx.Err()
		default:
		}

		r.log.Info("iteration_start", zap.Int("iter", r.result.Iterations+1))
		r.handledControl = false

		// An already-staged external call may be interrupted only through the
		// durable boundary (currently sleep). In every ordinary turn the previous
		// assistant result is settled first, preserving transcript causality.
		if r.agent.HasPendingExternalCall() {
			if _, err := r.drainBoundary(ctx); err != nil {
				return r.result, err
			}
		}

		done, err := r.handlePreviousResult(ctx)
		if err != nil {
			return r.result, err
		}

		accepted, err := r.drainBoundary(ctx)
		if err != nil {
			return r.result, err
		}

		if !accepted && (done || r.handledControl) {
			// Gated on the flag alone: an unconditional call would also run the
			// automatic threshold check on paths that just answered.
			if r.agent.compactionRequested() {
				r.applyContextEvents(ctx)
			}

			return r.result, nil
		}

		r.applyContextEvents(ctx)

		// Reload messages from DB to ensure in-memory is fresh after compaction.
		if err := r.agent.ms.reloadMessages(ctx); err != nil {
			r.log.Warn("reload_messages_failed", zap.Error(err))
		}

		if err := r.callLLM(ctx); err != nil {
			return r.result, err
		}

		if err := r.recordIteration(ctx); err != nil {
			return r.result, err
		}
	}

	return r.finalize(ctx)
}

func (r *loopRunner) handlePreviousResult(ctx context.Context) (bool, error) {
	// A call that is out with the world outranks everything: re-executing it
	// would apply the same change twice, and advancing past it would send the
	// provider a tool_use nothing answers.
	if r.agent.HasPendingExternalCall() {
		r.log.Info("session_suspended", zap.String("reason", "external call still pending"))
		r.result.Suspended = true

		return true, nil
	}

	state := lastAssistantState(r.agent.ms.getMessages())
	if state == nil {
		return false, nil
	}

	if state.HasPendingTools {
		r.emptyCount = 0 // a productive turn breaks the empty-response streak

		if state.HasText {
			r.notify(ctx, "🔄 "+state.Text)
		}

		r.log.Info("executing_pending_tools", zap.Int("count", len(state.PendingTools)))

		// An unrecorded tool result outranks the suspend flag — suspending here
		// would report a state the transcript does not back.
		if err := executeToolCalls(ctx, r.agent, state.PendingTools); err != nil {
			r.result.Error = err

			return false, fmt.Errorf("execute pending tools: %w", err)
		}

		if r.agent.suspended {
			r.log.Info("session_suspended", zap.String("reason", "tool requested suspend"))
			r.result.Suspended = true

			return true, nil
		}

		return false, nil
	}

	if state.HasText {
		// A terminal assistant message loaded at activation start is already
		// settled. It is state, not a new publication event. Only a response
		// produced by this runLoop invocation owns the transition that may notify
		// the human. Without this guard every later durable input would replay the
		// previous final answer before being promoted.
		if r.lastResp != nil {
			r.result.FinalResponse = state.Text
			r.notify(ctx, "✅ "+state.Text)
		}

		return true, nil
	}

	// Empty assistant (no text, no tools) — nudge
	r.emptyCount++
	r.log.Warn("empty_stop_response", zap.Int("iter", r.result.Iterations), zap.Int("consecutive", r.emptyCount))

	if r.emptyCount >= emptyResponseBreakThreshold {
		r.log.Warn("empty_response_notify_user", zap.Int("count", r.emptyCount))

		r.notifyPersistent(
			ctx,
			fmt.Sprintf(
				"⚠️ Model returned %d consecutive empty responses. Session paused — waiting for input.",
				r.emptyCount,
			),
		)

		return true, nil
	}

	nudge := "You returned an empty response with no tool calls. Please continue working on the task, or explain what you need."
	if r.emptyCount == emptyResponseWarnThreshold {
		nudge = fmt.Sprintf(
			"[AUTOMATED WARNING: You have returned %d consecutive empty responses (no text, no tool calls). You MUST either use a tool or respond with text. If you cannot proceed, explain why.]",
			r.emptyCount,
		)
	}

	if err := r.agent.ms.addUserMessage(ctx, nudge); err != nil {
		r.result.Error = err

		return false, fmt.Errorf("record empty-response nudge: %w", err)
	}

	return false, nil
}

// notify sends a message to the user without adding it to the model's conversation history.
func (r *loopRunner) notify(ctx context.Context, msg string) {
	if r.opts.Notify != nil {
		if err := r.opts.Notify(ctx, msg); err != nil {
			r.log.Warn("notify_failed", zap.Error(err))
		}
	}
}

func (r *loopRunner) notifyPersistent(ctx context.Context, msg string) {
	if err := r.agent.enqueuePersistentOutput(ctx, msg); err != nil {
		r.log.Warn("enqueue_output_failed", zap.Error(err))
	}

	r.notify(ctx, msg)
}

func (r *loopRunner) callLLM(ctx context.Context) error {
	activeTools := r.agent.registry.List()

	if r.agent.loopDetector.forceTextOnly {
		activeTools = nil

		r.log.Warn("force_text_only", zap.String("reason", "loop detector escalated to text-only mode"))
	}

	// Defensive: never send an unmatched tool_use to the LLM (would be a 400).
	// Pending external calls (sleep / blocking task) are excluded from stubbing —
	// the loop never reaches here with one pending, so this only guards stray
	// dangling calls from the compaction-adjacent edge.
	msgs := repairTranscriptExcluding(r.agent.ms.getMessages(), r.agent.pendingExternalCallIDs())

	system := r.agent.prompt.systemPrompt()
	schemas := tool.ToSchemas(activeTools)
	// The baseline indexes the in-memory transcript, not the repaired copy going
	// out: the delta is counted over the tail this position grows past.
	sentCount := len(r.agent.ms.getMessages())
	generation := r.agent.modelGeneration()

	response, err := r.agent.chat(ctx, system, msgs, schemas)
	if err != nil {
		r.log.Error("llm_call_failed", zap.Error(err))
		r.result.ErrorNotice = "❌ LLM error: " + logger.Redact(err.Error())

		r.result.Error = err

		return fmt.Errorf("LLM call failed: %w", err)
	}

	if response.Usage != nil {
		r.agent.recordContextBaseline(response.Usage.PromptTokens, sentCount, generation)
	}

	if r.agent.loopDetector.forceTextOnly && len(response.ToolCalls) == 0 {
		r.agent.loopDetector.clearForceTextOnly()
		r.log.Info("force_text_only_cleared", zap.String("reason", "LLM produced text response"))
	}

	r.lastResp = response

	return nil
}

func (r *loopRunner) recordIteration(ctx context.Context) error {
	r.result.Iterations++

	if r.cb != nil {
		if callbackErr := r.cb(r.result.Iterations, r.lastResp, r.lastResp.ToolCalls); callbackErr != nil {
			r.result.Error = callbackErr
			return fmt.Errorf("iteration callback failed: %w", callbackErr)
		}
	}

	outputType, output := assistantOutput(r.lastResp, r.agent.outputEnabled)
	if err := r.agent.ms.addAssistantMessageOutput(ctx, r.lastResp, outputType, output); err != nil {
		r.result.Error = err

		return fmt.Errorf("record assistant message: %w", err)
	}

	if r.lastResp.CostUSD > 0 {
		r.log.Info(
			"iteration_cost",
			zap.Int("iter", r.result.Iterations),
			zap.String("cost_usd", fmt.Sprintf("$%.4f", r.lastResp.CostUSD)),
		)
	}

	r.log.Info("iteration_end", zap.Int("iter", r.result.Iterations))

	return nil
}

func assistantOutput(response *llmwire.Response, enabled bool) (sessionstore.OutputType, string) {
	if !enabled || strings.TrimSpace(response.Text) == "" {
		return "", ""
	}

	if len(response.ToolCalls) > 0 {
		return sessionstore.OutputMessageReplaceable, response.Text
	}

	return sessionstore.OutputMessagePersistent, "✅ " + response.Text
}

func (r *loopRunner) finalize(ctx context.Context) (*loopResult, error) {
	if strings.TrimSpace(r.result.FinalResponse) == "" {
		if state := lastAssistantState(r.agent.ms.getMessages()); state != nil && state.HasText {
			r.result.FinalResponse = state.Text
		}
	}

	if strings.TrimSpace(r.result.FinalResponse) != "" && r.agent.outputEnabled {
		if err := r.agent.ms.enqueueFinalAssistantOutput(ctx, "✅ "+r.result.FinalResponse); err != nil {
			return r.result, err
		}
	}

	if strings.TrimSpace(r.result.FinalResponse) != "" && r.opts.Notify != nil {
		if err := r.opts.Notify(ctx, "✅ "+r.result.FinalResponse); err != nil {
			r.log.Warn("notify_failed", zap.Error(err))
		}
	}

	r.result.Error = fmt.Errorf("maximum iterations (%d) reached", r.maxIter)

	return r.result, r.result.Error
}

// lastAssistantState inspects the message history and returns the state of the
// last assistant message for resume/iteration handling.
func lastAssistantState(messages []llmwire.Message) *assistantState {
	if len(messages) == 0 {
		return nil
	}

	lastIdx := -1

	for i, v := range slices.Backward(messages) {
		if v.Role == llmwire.RoleAssistant {
			lastIdx = i
			break
		}

		if v.Role == llmwire.RoleUser {
			return nil
		}
	}

	if lastIdx < 0 {
		return nil
	}

	assistant := messages[lastIdx]

	if len(assistant.ToolCalls) == 0 {
		if strings.TrimSpace(assistant.Content) != "" {
			return &assistantState{HasText: true, Text: assistant.Content}
		}

		return &assistantState{}
	}

	resolvedIDs := make(map[string]bool)

	for i := lastIdx + 1; i < len(messages); i++ {
		if messages[i].Role == llmwire.RoleTool {
			resolvedIDs[messages[i].ToolCallID] = true
		}
	}

	var pending []llmwire.ToolCall

	for _, tc := range assistant.ToolCalls {
		if !resolvedIDs[tc.ID] {
			pending = append(pending, tc)
		}
	}

	if len(pending) > 0 {
		return &assistantState{
			HasPendingTools: true,
			PendingTools:    pending,
			HasText:         strings.TrimSpace(assistant.Content) != "",
			Text:            assistant.Content,
		}
	}

	return nil
}

package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	emptyResponseWarnThreshold  = 3
	emptyResponseBreakThreshold = 6
)

// hardIterationCeiling is an internal defect circuit breaker for a loop-detector
// blind spot, not a normal terminal — real runs end far below it.
const hardIterationCeiling = 1000

const maxInvalidToolCallAttempts = 3

const invalidToolCallRetryPrompt = "[AUTOMATED RECOVERY: The previous response used an ambiguous tool-call ID. " +
	"No tools from that response were executed. Retry the step with distinct call IDs.]"

const invalidToolCallTerminalPrompt = "[AUTOMATED RECOVERY: Tool-call IDs remained ambiguous after repeated attempts. " +
	"No tools from those responses were executed. Wait for new user input before trying again.]"

//nolint:gosec // prompt text shown to the model, not a credential
const loopWarningTemplate = `[LOOP WARNING: Low action diversity (%d%%). Your recent %d tool calls produced only %d unique outcomes.

REQUIRED: Before your next tool call, explain in text WHY your current approach is not working and WHAT specifically you will change. Do not repeat the same strategy.]`

const loopBlockMessage = `[BLOCKED: Tool execution blocked — you were warned about repetitive behavior but continued the same pattern. You MUST respond with text explaining your situation. No tool calls will be executed until you demonstrate a new approach.]`

const loopFailureWarningTemplate = `[LOOP WARNING: The %s tool has returned the same error %d times in a row. Repeating the identical call will not help — fix the arguments or change your approach, or stop and explain the problem in text.]`

type loopResult struct {
	FinalResponse          string
	ErrorNotice            string
	Iterations             int
	Error                  error
	Suspended              bool // true when a tool (e.g., sleep) requested session suspend
	TerminalStateCommitted bool
}

type iterationCallback func(
	iteration int,
	response *llmwire.Response,
	toolCalls []llmwire.ToolCall,
	alreadyPersisted bool,
) error

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
	Working   func(active bool)                               // main-model engagement; nil for subagents
}

// loopRunner holds per-run state for a single runLoop invocation.
type loopRunner struct {
	agent                *svc
	opts                 loopOptions
	cb                   iterationCallback
	result               *loopResult
	log                  *zap.Logger
	emptyStopTerminal    bool
	dispositionTerminal  bool
	confirmedFinal       bool
	lastResp             *llmwire.Response
	handledControl       bool
	replyToInput         bool
	directReplyEligible  bool
	publishedReply       bool
	acceptedManagerInput bool
	backgroundInserted   bool
	invalidToolCalls     int
}

//nolint:funlen,gocyclo,gocognit,wsl_v5 // Loop ordering is the session protocol.
func runLoop(ctx context.Context, agent *svc, opts loopOptions, callback iterationCallback) (*loopResult, error) {
	r := &loopRunner{
		agent:  agent,
		opts:   opts,
		cb:     callback,
		result: &loopResult{},
		log:    logger.Ctx(ctx).Named("session.loop"),
	}

	agent.contexts.beginRun()

	// Durable completion state resumes obligations, not decisions: the empty
	// streak escalates from the committed count the disposition transaction
	// reads, and the reply cache reconciles from the durable manager-reply
	// obligation.
	if agent.resumeCompletion != nil {
		r.replyToInput = agent.resumeCompletion.ManagerReplyPending
	}
	hb := newHeartbeatTicker(opts.Heartbeat)
	defer hb.stop()

	// Decision 21: every terminal exit resolves a still-pending grant exactly once,
	// so no provider error, the hard ceiling, empty pause, budget fire or stop can
	// wedge later inbox rows behind it.
	defer r.resolveTerminalGrant(ctx)
	if _, err := sessioncalls.Scan(agent.ms.getMessages()); err != nil {
		return r.result, fmt.Errorf("session transcript requires repair: %w", err)
	}

	hb.start(ctx)

	for r.result.Iterations < hardIterationCeiling {
		select {
		case <-ctx.Done():
			r.result.Error = ctx.Err()
			r.setWorking(false)

			return r.result, ctx.Err()
		default:
		}

		r.log.Info("iteration_start", zap.Int("iter", r.result.Iterations+1))
		r.handledControl = false
		r.acceptedManagerInput = false

		// An already-staged external call may be interrupted only through the
		// durable boundary (currently sleep). In every ordinary turn the previous
		// assistant result is settled first, preserving transcript causality.
		accepted := false
		if r.agent.HasPendingExternalCall() {
			var err error
			accepted, err = r.drainBoundary(ctx)
			if err != nil {
				return r.result, err
			}
		}

		done, err := r.handlePreviousResult(ctx)
		if err != nil {
			return r.result, err
		}

		acceptedAfterResult, err := r.drainBoundary(ctx)
		if err != nil {
			return r.result, err
		}
		accepted = accepted || acceptedAfterResult

		if !accepted && (done || r.handledControl) {
			// Gated on the flag alone: an unconditional call would also run the
			// automatic threshold check on paths that just answered.
			if r.agent.contexts.requested() {
				r.applyContextEvents(ctx)
			}
			r.parkBudget()

			return r.result, nil
		}

		// A final response cleared Working; a continued loop re-asserts it.
		r.setWorking(true)

		r.applyContextEvents(ctx)
		if r.parkBudget() {
			return r.result, nil
		}

		// Reload messages from DB to ensure in-memory is fresh after compaction.
		if err := r.agent.ms.reloadMessages(ctx); err != nil {
			r.log.Warn("reload_messages_failed", zap.Error(err))
		}

		if err := r.callLLM(ctx); err != nil {
			return r.result, err
		}
		if r.lastResp != nil && len(r.lastResp.ToolCalls) > 0 &&
			r.lastResp.FinishType != llmwire.FinishLength && r.lastResp.FinishType != llmwire.FinishUnknown {
			probe := append(r.agent.ms.getMessages(), llmwire.Message{
				Role: llmwire.RoleAssistant, ToolCalls: r.lastResp.ToolCalls,
			})
			if _, err := sessioncalls.Scan(probe); err != nil {
				r.result.Iterations++
				if _, recordErr := r.recordAmbiguousResponse(ctx); recordErr != nil {
					return r.result, recordErr
				}
				if r.parkBudget() {
					return r.result, nil
				}
				continue
			}
		}
		recoveryReply, err := r.hasOutstandingResponseRecovery(ctx)
		if err != nil {
			return r.result, err
		}

		if r.acceptedManagerInput {
			r.replyToInput = true
			r.directReplyEligible = true
		} else if recoveryReply {
			r.replyToInput = true
		}
		if r.parkBudget() {
			return r.result, nil
		}

		if err := r.recordIteration(ctx); err != nil {
			return r.result, err
		}
		if r.emptyStopTerminal || r.dispositionTerminal {
			// The disposition committed a durable terminal outcome (sixth
			// empty notice or projection-error settlement); the activation
			// ends through that commit — no further model call, and no
			// second generic error persistence over the committed one.
			// A committed projection error still reports its error so the
			// daemon settles the budget and notifies as an error, mirroring
			// the rejected-response terminal path.
			if r.result.Error != nil {
				return r.result, r.result.Error
			}

			return r.result, nil
		}
		if r.parkBudget() {
			return r.result, nil
		}
	}

	return r.finalize(ctx)
}

func (r *loopRunner) parkBudget() bool {
	if !r.agent.budgetFired {
		return false
	}

	r.result.Suspended = true
	r.setWorking(false)

	return true
}

//nolint:nestif,funlen // The prior-response protocol keeps suspend, tool, and terminal ordering together.
func (r *loopRunner) handlePreviousResult(ctx context.Context) (bool, error) {
	if _, err := sessioncalls.Scan(r.agent.ms.getMessages()); err != nil {
		return false, fmt.Errorf("session transcript requires repair: %w", err)
	}
	// A call that is out with the world outranks everything: re-executing it
	// would apply the same change twice, and advancing past it would send the
	// provider a tool_use nothing answers.
	if r.agent.HasPendingExternalCall() {
		r.log.Info("session_suspended", zap.String("reason", "external call still pending"))
		r.result.Suspended = true
		r.setWorking(false)

		return true, nil
	}

	state := lastAssistantState(r.agent.ms.getMessages())
	if state == nil {
		return false, nil
	}

	if state.HasPendingTools {
		if state.HasText {
			if r.publishedReply {
				r.notify(ctx, state.Text)
				r.publishedReply = false
			}

			message := "🔄 " + state.Text

			if provider, ok := r.agent.boundary.(progressChangeBoundary); ok && r.agent.outputEnabled {
				var published bool
				var progressErr error

				message, published, progressErr = provider.ProgressChange(ctx)
				if progressErr != nil && !errors.Is(progressErr, sessionstore.ErrProgressSuperseded) {
					return false, fmt.Errorf("enqueue model progress snapshot: %w", progressErr)
				}

				if !published {
					message = ""
				}
			}

			if message != "" {
				r.notify(ctx, message)
			}
		}

		r.log.Info("executing_pending_tools", zap.Int("count", len(state.PendingTools)))

		// An unrecorded tool result outranks the suspend flag — suspending here
		// would report a state the transcript does not back.
		outcome, err := r.agent.turns.Execute(ctx, state.PendingTools, r.agent.currentActivation, r.opts.Notify)

		r.agent.suspended = outcome.suspended
		if outcome.consumedGrant {
			r.agent.currentActivation = nil
		}

		if err != nil {
			r.result.Error = err

			return false, fmt.Errorf("execute pending tools: %w", err)
		}

		if r.agent.suspended {
			r.log.Info("session_suspended", zap.String("reason", "tool requested suspend"))
			r.result.Suspended = true
			r.setWorking(false)

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
			if err := r.expireCurrentActivation(ctx); err != nil {
				return false, err
			}

			// The final response ends the loop's engagement: clear Working before
			// the message goes out so later cards read background work, not working.
			r.setWorking(false)
			// A confirmed completion check settles FinalResponse on the
			// candidate; this stop's own ack text is not echoed. The flag is
			// consumed here so any later stop owns its own echo.
			if !r.confirmedFinal {
				r.result.FinalResponse = state.Text
			}

			r.confirmedFinal = false

			r.notify(ctx, r.result.FinalResponse)
		}

		return true, nil
	}

	return false, nil
}

func (r *loopRunner) expireCurrentActivation(ctx context.Context) error {
	if r.agent.currentActivation == nil {
		return nil
	}

	resolver, ok := r.agent.boundary.(activationResolver)
	if !ok {
		return errors.New("activation resolver unavailable")
	}

	if err := resolver.ExpireActivation(ctx, *r.agent.currentActivation); err != nil {
		return fmt.Errorf("expire unused activation: %w", err)
	}

	r.agent.currentActivation = nil

	return nil
}

// resolveTerminalGrant runs once per runLoop exit. A consumed grant is left
// alone: it belongs to the owed-call replay contract, not to expiry. A suspend
// whose unresolved tool call is the activated tool's own is left alone too —
// the daemon spends that grant when the staged mutation commits, and a terminal
// receipt here would falsely claim the command did nothing.
func (r *loopRunner) resolveTerminalGrant(ctx context.Context) {
	grant := r.agent.currentActivation
	if grant == nil || grant.ToolCallID != "" {
		return
	}

	if r.result.Suspended && ctx.Err() == nil && r.activationCallPending() {
		return
	}

	runCtx := context.WithoutCancel(ctx)

	if ctx.Err() != nil {
		// A cancelled context means stop/kill/shutdown: their lifecycle output
		// answers the command turn, so the store-only expiry avoids a receipt.
		canceler, ok := r.agent.boundary.(activationCancelBoundary)
		if !ok {
			return
		}

		if err := canceler.CancelActivation(runCtx, *grant); err != nil {
			r.log.Warn("cancel_activation_failed", zap.Error(err))

			return
		}

		r.agent.currentActivation = nil

		return
	}

	if err := r.expireCurrentActivation(runCtx); err != nil {
		r.log.Warn("expire_activation_on_terminal_failed", zap.Error(err))
	}
}

// activationCallPending reports whether the suspended turn leaves the activated
// tool's call unanswered — the daemon still owns that call's settlement.
func (r *loopRunner) activationCallPending() bool {
	pending := unresolvedToolCalls(r.agent.ms.getMessages())

	for _, toolID := range pending {
		if toolID == r.agent.currentActivation.ToolID {
			return true
		}
	}

	return false
}

// notify sends a message to the user without adding it to the model's conversation history.
func (r *loopRunner) notify(ctx context.Context, msg string) {
	if r.opts.Notify != nil {
		if err := r.opts.Notify(ctx, msg); err != nil {
			r.log.Warn("notify_failed", zap.Error(err))
		}
	}
}

// setWorking reports main-model engagement to the host. A final response ends
// the engagement before it is published; a continued loop re-asserts it.
func (r *loopRunner) setWorking(active bool) {
	if r.opts.Working != nil {
		r.opts.Working(active)
	}
}

func (r *loopRunner) notifyPersistent(ctx context.Context, msg string) {
	if err := r.agent.enqueuePersistentOutput(ctx, msg); err != nil {
		r.log.Warn("enqueue_output_failed", zap.Error(err))
	}

	r.notify(ctx, msg)
}

func (r *loopRunner) callLLM(ctx context.Context) error {
	r.lastResp = nil
	if r.agent.budgetGate != nil {
		if err := r.agent.budgetGate.Admit(ctx, time.Now().UTC()); err != nil {
			if errors.Is(err, ErrBudgetCheckpoint) {
				r.agent.budgetFired = true

				return nil
			}

			return fmt.Errorf("budget admission: %w", err)
		}
	}

	if !r.backgroundInserted && r.agent.activeBackgroundSnapshot != "" {
		if err := r.agent.ms.addUserMessage(ctx, r.agent.activeBackgroundSnapshot); err != nil {
			return fmt.Errorf("record active background snapshot: %w", err)
		}

		r.backgroundInserted = true
	}

	if !r.agent.turns.toolsAllowed() {
		r.log.Warn("force_text_only", zap.String("reason", "loop detector escalated to text-only mode"))
	}

	// Defensive: never send an unmatched tool_use to the LLM (would be a 400).
	// Pending external calls (sleep / blocking task) are excluded from stubbing —
	// the loop never reaches here with one pending, so this only guards stray
	// dangling calls from the compaction-adjacent edge.
	active := r.agent.ms.getMessages()
	if _, err := sessioncalls.Scan(active); err != nil {
		return fmt.Errorf("session transcript requires repair: %w", err)
	}

	msgs := repairTranscriptExcluding(active, r.agent.pendingExternalCallIDs())

	system := r.agent.prompt.systemPrompt()
	schemas := r.agent.turns.schemas()
	// The baseline indexes the in-memory transcript, not the repaired copy going
	// out: the delta is counted over the tail this position grows past.
	sentCount := len(r.agent.ms.getMessages())
	generation := r.agent.models.snapshot().generation

	response, err := r.agent.models.Chat(ctx, system, msgs, schemas)
	if err != nil {
		r.log.Error("llm_call_failed", zap.Error(err))
		r.result.ErrorNotice = "❌ LLM error: " + logger.Redact(err.Error())

		r.result.Error = err

		return fmt.Errorf("LLM call failed: %w", err)
	}

	if response.Usage != nil {
		r.agent.models.recordBaseline(ctx, response.Usage.PromptTokens, sentCount, generation)
	}

	response.FinishType = normalizedFinishType(response.FinishType)

	if len(response.ToolCalls) == 0 && r.agent.turns.observeText() {
		r.log.Info("force_text_only_cleared", zap.String("reason", "LLM produced text response"))
	}

	r.lastResp = response

	return nil
}

func normalizedFinishType(finishType string) string {
	switch finishType {
	case llmwire.FinishStop, llmwire.FinishToolCalls, llmwire.FinishLength, llmwire.FinishUnknown:
		return finishType
	default:
		return llmwire.FinishUnknown
	}
}

func (r *loopRunner) recordIteration(ctx context.Context) error {
	r.result.Iterations++

	if r.lastResp.FinishType == llmwire.FinishLength || r.lastResp.FinishType == llmwire.FinishUnknown {
		if err := r.recordRejectedIteration(ctx); err != nil {
			return err
		}

		r.directReplyEligible = false

		return nil
	}
	handled, err := r.recordAmbiguousResponse(ctx)
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	return r.recordDispositionIteration(ctx)
}

//nolint:nestif,wsl_v5 // This preflight retains one malformed attempt before any tool can execute.
func (r *loopRunner) recordAmbiguousResponse(ctx context.Context) (bool, error) {
	if len(r.lastResp.ToolCalls) == 0 {
		return false, nil
	}

	probe := append(r.agent.ms.getMessages(), llmwire.Message{
		Role: llmwire.RoleAssistant, ToolCalls: r.lastResp.ToolCalls,
	})
	if _, err := sessioncalls.Scan(probe); err != nil {
		state, stateErr := r.completionState(ctx)
		if stateErr != nil {
			return true, stateErr
		}
		nextAttempts := r.invalidToolCalls + 1
		prompt := invalidToolCallRetryPrompt
		if nextAttempts >= maxInvalidToolCallAttempts {
			prompt = invalidToolCallTerminalPrompt
		}
		decision := dispositionDecision{
			kind: sessionstore.ResponseDispositionToolCall, unusableCalls: true,
			expectedCandidate: durableCandidateID(state),
			nudge:             hostUserMessage(prompt),
		}
		result, commitErr := r.commitDisposition(ctx, decision, state)
		if commitErr != nil {
			return true, fmt.Errorf("persist unusable assistant response: %w", commitErr)
		}
		if r.cb != nil {
			if callbackErr := r.cb(r.result.Iterations, r.lastResp, r.lastResp.ToolCalls, true); callbackErr != nil {
				if result.BudgetFired {
					r.log.Warn("unusable_response_callback_failed", zap.Error(callbackErr))
				} else {
					return true, fmt.Errorf("unusable response callback: %w", callbackErr)
				}
			}
		}
		if result.BudgetFired {
			r.result.Suspended = true
			r.setWorking(false)
			return true, nil
		}
		r.invalidToolCalls = nextAttempts
		if r.invalidToolCalls < maxInvalidToolCallAttempts {
			return true, nil
		}
		r.result.Error = fmt.Errorf("model returned ambiguous tool-call IDs %d times: %w", nextAttempts, err)
		r.result.ErrorNotice = "⚠️ Model repeatedly returned ambiguous tool-call IDs. No tools from those responses ran; send a new message to retry."
		return true, r.result.Error
	}
	return false, nil
}

func assistantOutput(
	response *llmwire.Response,
	enabled, replyToInput, directReplyEligible bool,
) (sessionstore.OutputType, string) {
	if !enabled || strings.TrimSpace(response.Text) == "" {
		return "", ""
	}

	if len(response.ToolCalls) > 0 {
		if directReplyEligible {
			return sessionstore.OutputMessagePersistent, response.Text
		}

		return "", ""
	}

	if replyToInput {
		return sessionstore.OutputMessagePersistent, response.Text
	}

	return sessionstore.OutputMessageReplaceable, response.Text
}

func (r *loopRunner) finalize(ctx context.Context) (*loopResult, error) {
	if strings.TrimSpace(r.result.FinalResponse) == "" {
		if state := lastAssistantState(r.agent.ms.getMessages()); state != nil && state.HasText {
			r.result.FinalResponse = state.Text
		}
	}

	if strings.TrimSpace(r.result.FinalResponse) != "" && r.agent.outputEnabled {
		if err := r.agent.ms.enqueueFinalAssistantOutput(ctx, r.result.FinalResponse); err != nil {
			return r.result, err
		}
	}

	// Append progress footer to final response before notifying.
	footer := r.result.FinalResponse

	if renderer, ok := r.agent.boundary.(finalOutputBoundary); ok {
		var err error

		footer, err = renderer.FinalOutput(ctx, footer)
		if err != nil {
			r.log.Warn("render_final_output_failed", zap.Error(err))
			footer = r.result.FinalResponse
		}
	}

	if strings.TrimSpace(footer) != "" && r.opts.Notify != nil {
		// The ceiling response is the loop's final word; clear Working first.
		r.setWorking(false)

		if err := r.opts.Notify(ctx, footer); err != nil {
			r.log.Warn("notify_failed", zap.Error(err))
		}
	}

	r.result.Error = fmt.Errorf("maximum iterations (%d) reached", hardIterationCeiling)

	return r.result, r.result.Error
}

// lastAssistantState inspects the message history and returns the state of the
// last assistant message for resume/iteration handling. Any trailing user
// message — including the host completion nudge — ends the scan: the nudge
// must reach the model so a pending candidate gets its confirmation call.
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
		if assistant.FinishType == llmwire.FinishToolCalls {
			// Empty-response recovery: the body is not a final answer even
			// when text rides along, so it stays hidden from publication.
			return &assistantState{}
		}

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

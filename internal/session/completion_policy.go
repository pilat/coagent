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
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// ResponseDispositionStore is the durable boundary one accepted assistant
// response commits through: message, completion check, empty streak, budget
// verdict, host nudge, and optional manager output in one SQLite transition.
type ResponseDispositionStore = sessionstore.ResponseDispositionStore

// AcceptedResponseResult is the committed identity set one disposition produced.
type AcceptedResponseResult = sessionstore.AcceptedResponseResult

// dispositionDecision is the loop's chosen transition for one accepted
// response, resolved from finish integrity, tool shape, wake projection, and
// the durable completion check state.
type dispositionDecision struct {
	kind    sessionstore.ResponseDispositionKind
	output  string
	outType sessionstore.OutputType
	// expectedCandidate validates confirm/clear transitions; zero means the
	// caller expects no pending check.
	expectedCandidate int64
	// nudge is the typed host user-role message the disposition inserts in
	// the same transaction (candidate and empty-stop kinds).
	nudge *transcript.Message
}

// decideDisposition classifies one accepted response after finish-integrity
// routing. Tool-bearing responses always clear any pending check. A no-tool
// stop yields immediately on a wake source; otherwise the two-phase check
// hides the first candidate and publishes the second. A tool_calls finish
// with no calls never reaches this dispatch: it follows empty-response
// recovery in recordDispositionIteration.
func (r *loopRunner) decideDisposition(
	ctx context.Context,
	state *sessionstore.CompletionCheckState,
) dispositionDecision {
	if len(r.lastResp.ToolCalls) > 0 {
		decision := dispositionDecision{
			kind:              sessionstore.ResponseDispositionToolCall,
			expectedCandidate: durableCandidateID(state),
		}

		decision.outType, decision.output = assistantOutput(
			r.lastResp, r.agent.outputEnabled, r.replyToInput, r.directReplyEligible,
		)

		return decision
	}

	wake, wakeErr := r.wakePresent(ctx)
	if wakeErr != nil {
		return dispositionDecision{
			kind:   sessionstore.ResponseDispositionProjectionError,
			output: projectionErrorNotice(wakeErr),
		}
	}

	if wake {
		decision := dispositionDecision{kind: sessionstore.ResponseDispositionBackgroundYield}
		decision.outType, decision.output = assistantOutput(
			r.lastResp, r.agent.outputEnabled, r.replyToInput, r.directReplyEligible,
		)

		return decision
	}

	if durableCandidateID(state) == 0 {
		return dispositionDecision{
			kind:  sessionstore.ResponseDispositionCandidate,
			nudge: r.nudgeMessage(),
		}
	}

	decision := dispositionDecision{
		kind:              sessionstore.ResponseDispositionConfirmed,
		expectedCandidate: durableCandidateID(state),
	}
	decision.outType, decision.output = assistantOutput(
		r.lastResp, r.agent.outputEnabled, r.replyToInput, r.directReplyEligible,
	)

	// The pending candidate is the full considered answer; the confirming
	// stop's own text is by construction a terse "why I'm stopping" ack and is
	// discarded. Type and releasing semantics stay as they are; an
	// output-disabled child still publishes nothing (owner guard), but its
	// recovery value is the candidate.
	if state != nil && strings.TrimSpace(state.CandidateText) != "" {
		decision.output = state.CandidateText
	}

	return decision
}

// wakePresent projects the exact session's durable background wake source.
// The projection is fail-closed: a boundary without the wake capability or a
// lookup error is never "no" — the disposition commits a projection error and
// retains the attempt instead of guessing.
func (r *loopRunner) wakePresent(ctx context.Context) (bool, error) {
	projection, ok := r.agent.boundary.(WakeSourceBoundary)
	if !ok {
		return false, errors.New("wake source projection unavailable")
	}

	has, err := projection.HasBackgroundWakeSource(ctx)
	if err != nil {
		return false, fmt.Errorf("query background wake source: %w", err)
	}

	return has, nil
}

func durableCandidateID(state *sessionstore.CompletionCheckState) int64 {
	if state == nil || state.CandidateID == nil {
		return 0
	}

	return *state.CandidateID
}

// projectionErrorNotice renders the host error the disposition commits with
// the retained attempt when the wake projection cannot answer. The wording
// matches the loop's generic session-error contract.
func projectionErrorNotice(err error) string {
	return fmt.Sprintf(
		"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
		logger.Redact(err.Error()),
	)
}

// nudgeMessage builds the typed host completion nudge for a hidden candidate.
// The role is user; the loop never recognizes it by its English text.
func (r *loopRunner) nudgeMessage() *transcript.Message {
	return hostUserMessage(renderCompletionNudge(r.agent.todoStore.List()))
}

// hostUserMessage wraps host-authored continuation text as a user-role
// transcript row.
func hostUserMessage(content string) *transcript.Message {
	return &transcript.Message{Role: "user", Content: content}
}

// recordDispositionIteration persists one accepted response through the
// durable disposition transaction. Empty no-wake stops route to the empty
// policy; every other accepted response commits its decided disposition.
func (r *loopRunner) recordDispositionIteration(ctx context.Context) error {
	replyToInput := r.replyToInput

	if r.cb != nil {
		if callbackErr := r.cb(r.result.Iterations, r.lastResp, r.lastResp.ToolCalls, false); callbackErr != nil {
			r.result.Error = callbackErr

			return fmt.Errorf("iteration callback failed: %w", callbackErr)
		}
	}

	state, stateErr := r.completionState(ctx)
	if stateErr != nil {
		return stateErr
	}

	// Decision 11: the reply cache is initialized and reconciled from the
	// durable fact, so a confirmed response after a hidden candidate still
	// publishes as the persistent, releasing manager answer.
	if state != nil && state.ManagerReplyPending {
		r.replyToInput = true
	}

	// An empty no-tool stop is a no-progress signal, not confirmation: the
	// empty policy counts it toward the 3/6 escalation. A tool_calls finish
	// with no calls follows empty-response recovery too — its body, even when
	// text rides along, is never a final answer.
	if len(r.lastResp.ToolCalls) == 0 &&
		(r.lastResp.FinishType == llmwire.FinishToolCalls ||
			strings.TrimSpace(r.lastResp.Text) == "") {
		return r.recordEmptyStopDisposition(ctx, state)
	}

	decision := r.decideDisposition(ctx, state)

	result, err := r.commitDisposition(ctx, decision, state)
	if err != nil {
		return fmt.Errorf("commit accepted response disposition: %w", err)
	}

	r.afterCommittedDisposition(decision, result, replyToInput)

	r.log.Info("iteration_end", zap.Int("iter", r.result.Iterations))

	return nil
}

// recordEmptyStopDisposition persists one empty no-tool stop through the
// durable empty-streak policy. A wake source yields immediately at any prior
// count. Without wake the streak increments: ordinary nudges below three, the
// strong warning at three, and the terminal host notice at six, which ends
// the activation through ordinary successful completion.
func (r *loopRunner) recordEmptyStopDisposition(
	ctx context.Context,
	state *sessionstore.CompletionCheckState,
) error {
	wake, wakeErr := r.wakePresent(ctx)
	if wakeErr != nil {
		decision := dispositionDecision{
			kind:   sessionstore.ResponseDispositionProjectionError,
			output: projectionErrorNotice(wakeErr),
		}

		result, err := r.commitDisposition(ctx, decision, state)
		if err != nil {
			return fmt.Errorf("commit projection error disposition: %w", err)
		}

		r.afterCommittedDisposition(decision, result, r.replyToInput)

		return nil
	}

	if wake {
		decision := dispositionDecision{
			kind:    sessionstore.ResponseDispositionBackgroundYield,
			outType: "",
		}

		result, err := r.commitDisposition(ctx, decision, state)
		if err != nil {
			return fmt.Errorf("commit background yield disposition: %w", err)
		}

		// An empty response carries no text for handlePreviousResult to
		// settle, so the activation must end here instead of calling the
		// model again (decision 4: an empty stop on this path yields
		// immediately).
		r.afterCommittedDisposition(decision, result, r.replyToInput)
		r.dispositionTerminal = true

		return nil
	}

	next := 1
	if state != nil {
		next = state.EmptyStopStreak + 1
	}

	if next >= emptyResponseBreakThreshold {
		return r.commitTerminalEmptyStop(ctx, state, next)
	}

	// The empty-stop nudge rides the disposition transaction: a crash cannot
	// strand a durable streak increment without its model-visible warning.
	decision := dispositionDecision{
		kind:              sessionstore.ResponseDispositionEmptyStop,
		expectedCandidate: durableCandidateID(state),
		nudge:             hostUserMessage(emptyStopNudge(next)),
	}

	if _, err := r.commitDisposition(ctx, decision, state); err != nil {
		return fmt.Errorf("commit empty stop disposition: %w", err)
	}

	r.log.Warn("empty_stop_response",
		zap.Int("iter", r.result.Iterations), zap.Int("consecutive", next))

	return nil
}

// emptyStopNudge renders the ordinary empty-response notice: the established
// strong warning at three, the plain continuation nudge otherwise.
func emptyStopNudge(count int) string {
	if count == emptyResponseWarnThreshold {
		return fmt.Sprintf(
			"[AUTOMATED WARNING: You have returned %d consecutive empty responses (no text, no tool calls). You MUST either use a tool or respond with text. If you cannot proceed, explain why.]",
			count,
		)
	}

	return "You returned an empty response with no tool calls. Please continue working on the task, or explain what you need."
}

// commitTerminalEmptyStop commits the sixth empty response as the ordinary
// successful final result: one durable host notice through the disposition's
// idempotent outbox row. A committed row is announced once; a child or
// output-disabled session stays silent.
func (r *loopRunner) commitTerminalEmptyStop(
	ctx context.Context,
	state *sessionstore.CompletionCheckState,
	next int,
) error {
	notice := sessionstore.EmptyStopTerminalNotice(next)

	decision := dispositionDecision{
		kind:              sessionstore.ResponseDispositionEmptyStop,
		expectedCandidate: durableCandidateID(state),
		output:            notice,
	}

	result, err := r.commitDisposition(ctx, decision, state)
	if err != nil {
		return fmt.Errorf("commit terminal empty stop: %w", err)
	}

	r.emptyStopTerminal = true
	r.log.Warn("empty_response_notify_user", zap.Int("count", next))

	if result.Output != nil {
		r.notify(ctx, notice)
	}

	return nil
}

// completionState loads the durable check, reply obligation, and empty streak
// the loop turn resumes from. Without a store the loop stays in-memory.
func (r *loopRunner) completionState(ctx context.Context) (*sessionstore.CompletionCheckState, error) {
	if r.agent.dispositions == nil {
		return nil, nil //nolint:nilnil // nil state is the in-memory loop marker.
	}

	state, err := r.agent.dispositions.LoadCompletionCheckState(ctx, r.agent.id)
	if err != nil {
		return nil, fmt.Errorf("load completion check state: %w", err)
	}

	return state, nil
}

// afterCommittedDisposition updates loop-local caches from the committed
// outcome. The human-facing announcement stays on handlePreviousResult: it
// reads the settled transcript and works for both disposition paths, so the
// disposition commit never publishes prose twice.
func (r *loopRunner) afterCommittedDisposition(
	decision dispositionDecision,
	result *sessionstore.AcceptedResponseResult,
	replyToInput bool,
) {
	r.replyToInput = replyToInput && len(r.lastResp.ToolCalls) > 0
	r.directReplyEligible = false
	r.publishedReply = result.Output != nil && !result.BudgetFired &&
		decision.kind != sessionstore.ResponseDispositionCandidate
	r.dispositionTerminal = result.TerminalCommitted

	// A terminal-committed disposition already wrote the durable terminal
	// state (error status plus its host notice): run() must not write a
	// second, overriding status afterward.
	if result.TerminalCommitted {
		r.result.TerminalStateCommitted = true
		r.result.ErrorNotice = decision.output
		r.result.Error = errors.New(decision.output)
	}

	// Only a completed transcript is a final response; a budget checkpoint or
	// a terminal error never publishes the model text through run()'s result.
	// The published content is decision.output — the candidate text on a
	// confirmed check, not the nudge ack.
	if decision.kind == sessionstore.ResponseDispositionConfirmed {
		r.result.FinalResponse = decision.output
		r.confirmedFinal = true
	}
}

func (r *loopRunner) commitDisposition(
	ctx context.Context,
	decision dispositionDecision,
	state *sessionstore.CompletionCheckState,
) (*sessionstore.AcceptedResponseResult, error) {
	message := llmwire.Message{
		Role: llmwire.RoleAssistant, Content: r.lastResp.Text, ToolCalls: r.lastResp.ToolCalls,
		ReasoningContent: r.lastResp.ReasoningContent, ReasoningRaw: r.lastResp.ReasoningRaw,
		CostUSD: r.lastResp.CostUSD, Usage: r.lastResp.Usage,
		FinishType: r.lastResp.FinishType, ProviderFinishReason: r.lastResp.ProviderFinishReason,
	}

	stored, err := storedMessage(&message)
	if err != nil {
		return nil, fmt.Errorf("serialize accepted response: %w", err)
	}

	disposition := sessionstore.AcceptedResponseDisposition{
		SessionID: r.agent.id, RootID: r.agent.rootID,
		Iteration: r.agent.iterationOffset + r.result.Iterations,
		Message:   stored, Kind: decision.kind,
		Output:              decision.output,
		OutputType:          decision.outType,
		ExpectedCandidateID: decision.expectedCandidate,
		EmptyStopStreak:     r.emptyStreakAfter(decision, state),
		ManagerReplyPending: r.replyPendingAfter(state),
		ObservedAt:          time.Now().UTC(),
	}
	disposition.RenderFinal = r.renderDispositionFinal(decision, disposition.ObservedAt)

	disposition.Nudge = decision.nudge

	result, err := r.agent.dispositions.CommitAcceptedResponseDisposition(ctx, disposition)
	if err != nil {
		return nil, fmt.Errorf("persist accepted response disposition: %w", err)
	}

	if adoptErr := r.adoptCommittedDisposition(ctx, result); adoptErr != nil {
		return nil, adoptErr
	}

	return result, nil
}

// renderDispositionFinal returns the pure final-composition hook the store
// applies inside the disposition transaction: post-disposition facts rendered
// database-free. The footer timestamp is the disposition's observed time, so
// identical retries render identical content. A broken todo projection
// degrades to the raw text instead of hiding a confirmed answer behind the
// footer's failure.
func (r *loopRunner) renderDispositionFinal(
	decision dispositionDecision,
	observedAt time.Time,
) func(*sessionstore.ProgressFacts) string {
	return func(facts *sessionstore.ProgressFacts) string {
		composed, err := finalFactsFromProgress(facts, observedAt)
		if err != nil {
			return decision.output
		}

		composed.IsBackgroundYield = decision.kind == sessionstore.ResponseDispositionBackgroundYield

		return progress.RenderFinalFromFacts(decision.output, composed)
	}
}

// adoptCommittedDisposition updates the in-memory projection after the
// database commit: the transcript rows reload from the authoritative store
// and the budget cache adopts the committed verdict.
func (r *loopRunner) adoptCommittedDisposition(
	ctx context.Context,
	result *sessionstore.AcceptedResponseResult,
) error {
	if err := r.agent.ms.reloadMessages(ctx); err != nil {
		return err
	}

	r.agent.budgetFired = result.BudgetFired
	if result.BudgetFired && r.agent.budgetGate != nil {
		// The disposition commit replaced PersistResponse, so its fired
		// verdict still must reach the host park scheduler synchronously.
		r.agent.budgetGate.BudgetFired(result.Budget)
	}

	return nil
}

// emptyStreakAfter projects the durable trailing count after this response.
// Only a no-wake empty attempt advances the streak. An empty wake yield
// preserves the prior count; a non-empty yield resets it like any other
// non-empty accepted response.
func (r *loopRunner) emptyStreakAfter(
	decision dispositionDecision,
	state *sessionstore.CompletionCheckState,
) int {
	prior := 0
	if state != nil {
		prior = state.EmptyStopStreak
	}

	if decision.kind == sessionstore.ResponseDispositionEmptyStop {
		return prior + 1
	}

	if decision.kind == sessionstore.ResponseDispositionBackgroundYield &&
		strings.TrimSpace(r.lastResp.Text) == "" {
		return prior
	}

	return 0
}

// replyPendingAfter carries the durable manager-reply obligation through one
// disposition. Only a releasing confirmed/yield output clears it; the store
// applies that clearing when its output commits.
func (r *loopRunner) replyPendingAfter(
	state *sessionstore.CompletionCheckState,
) bool {
	if state != nil && state.ManagerReplyPending {
		return true
	}

	return r.acceptedManagerInput
}

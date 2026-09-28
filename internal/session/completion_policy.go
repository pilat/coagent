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

type responseFacts struct {
	response            *llmwire.Response
	candidateID         int64
	candidateText       string
	wake                bool
	projectionError     string
	emptyStreak         int
	outputEnabled       bool
	replyPending        bool
	directReplyEligible bool
	completionNudge     string
}

// classifyResponse proposes a transition; the disposition transaction validates its CAS and budget.
func classifyResponse(f responseFacts) dispositionDecision {
	decision := dispositionDecision{expectedCandidate: f.candidateID}

	decision.outType, decision.output = assistantOutput(
		f.response,
		f.outputEnabled,
		f.replyPending,
		f.directReplyEligible,
	)
	if len(f.response.ToolCalls) > 0 {
		decision.kind = sessionstore.ResponseDispositionToolCall
		return decision
	}

	if f.projectionError != "" {
		return dispositionDecision{kind: sessionstore.ResponseDispositionProjectionError, output: f.projectionError}
	}

	empty := f.response.FinishType == llmwire.FinishToolCalls || strings.TrimSpace(f.response.Text) == ""
	if f.wake {
		decision.kind = sessionstore.ResponseDispositionBackgroundYield

		decision.expectedCandidate = 0
		if empty {
			decision.output, decision.outType = "", ""
		}

		return decision
	}

	if empty {
		decision.kind = sessionstore.ResponseDispositionEmptyStop
		decision.outType, decision.output = "", ""

		next := f.emptyStreak + 1
		if next >= emptyResponseBreakThreshold {
			decision.output = sessionstore.EmptyStopTerminalNotice(next)
		} else {
			decision.nudge = hostUserMessage(emptyStopNudge(next))
		}

		return decision
	}

	if f.candidateID == 0 {
		return dispositionDecision{
			kind:  sessionstore.ResponseDispositionCandidate,
			nudge: hostUserMessage(f.completionNudge),
		}
	}

	decision.kind = sessionstore.ResponseDispositionConfirmed
	if strings.TrimSpace(f.candidateText) != "" {
		decision.output = f.candidateText
	}

	return decision
}

func (r *loopRunner) decideDisposition(
	ctx context.Context,
	state *sessionstore.CompletionCheckState,
) dispositionDecision {
	facts := responseFacts{
		response: r.lastResp, candidateID: durableCandidateID(state),
		outputEnabled: r.agent.outputEnabled, replyPending: r.replyToInput,
		directReplyEligible: r.directReplyEligible,
	}
	if state != nil {
		facts.candidateText, facts.emptyStreak = state.CandidateText, state.EmptyStopStreak
	}

	if len(r.lastResp.ToolCalls) == 0 {
		var wakeErr error

		facts.wake, wakeErr = r.wakePresent(ctx)
		if wakeErr != nil {
			facts.projectionError = projectionErrorNotice(wakeErr)
		}

		if !facts.wake && facts.projectionError == "" && facts.candidateID == 0 &&
			r.lastResp.FinishType != llmwire.FinishToolCalls && strings.TrimSpace(r.lastResp.Text) != "" {
			facts.completionNudge = renderCompletionNudge(r.agent.todoStore.List())
		}
	}

	return classifyResponse(facts)
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

	decision := r.decideDisposition(ctx, state)

	result, err := r.commitDisposition(ctx, decision, state)
	if err != nil {
		return fmt.Errorf("commit accepted response disposition: %w", err)
	}

	if decision.kind == sessionstore.ResponseDispositionEmptyStop {
		next := state.EmptyStopStreak + 1

		r.emptyStopTerminal = !result.BudgetFired && next >= emptyResponseBreakThreshold
		if r.emptyStopTerminal && result.Output != nil {
			r.notify(ctx, decision.output)
		}

		r.log.Warn("empty_stop_response", zap.Int("iter", r.result.Iterations), zap.Int("consecutive", next))
	} else if !result.BudgetFired {
		r.afterCommittedDisposition(decision, result, replyToInput)
	}

	if !result.BudgetFired && decision.kind == sessionstore.ResponseDispositionBackgroundYield &&
		(r.lastResp.FinishType == llmwire.FinishToolCalls || strings.TrimSpace(r.lastResp.Text) == "") {
		r.dispositionTerminal = true
	}

	r.log.Info("iteration_end", zap.Int("iter", r.result.Iterations))

	if r.lastResp.CostUSD > 0 {
		r.log.Info("iteration_cost", zap.Int("iter", r.result.Iterations),
			zap.String("cost_usd", fmt.Sprintf("$%.4f", r.lastResp.CostUSD)))
	}

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

// completionState loads the durable check, reply obligation, and empty streak
// the loop turn resumes from.
func (r *loopRunner) completionState(ctx context.Context) (*sessionstore.CompletionCheckState, error) {
	state, err := r.agent.dispositions.LoadCompletionCheckState(ctx, r.agent.id)
	if err != nil {
		return nil, fmt.Errorf("load completion check state: %w", err)
	}

	return state, nil
}

// The transcript-driven announcement stays in handlePreviousResult to avoid duplicate publication.
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
	r.agent.budgetFired = result.BudgetFired
	if result.BudgetFired && r.agent.budgetGate != nil {
		// The committed verdict must reach the host park scheduler synchronously.
		r.agent.budgetGate.BudgetFired(result.Budget)
	}

	if err := r.agent.ms.reloadMessages(ctx); err != nil {
		if result.BudgetFired {
			// Parking consumes no further transcript input; the next activation reloads it.
			r.log.Warn("budget_transcript_reload_failed", zap.Error(err))

			return nil
		}

		return err
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

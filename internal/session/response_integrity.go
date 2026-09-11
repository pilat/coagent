package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (r *loopRunner) recordRejectedIteration(ctx context.Context) error {
	rejection, err := r.rejectedResponse()
	if err != nil {
		return err
	}

	result, err := r.persistRejectedResponse(ctx, rejection)
	if err != nil {
		return fmt.Errorf("persist rejected response: %w", err)
	}

	if err := r.applyRejectedOutcome(result); err != nil {
		return err
	}

	if err := r.reportRejectedIteration(); err != nil {
		return err
	}

	r.log.Info("iteration_end", zap.Int("iter", r.result.Iterations))

	switch result.Outcome {
	case sessionstore.RejectedResponseRecoveryQueued, sessionstore.RejectedResponseBudgetSuppressed:
		return nil
	case sessionstore.RejectedResponseRetryExhausted, sessionstore.RejectedResponseUnknownTerminal:
		return r.result.Error
	default:
		return fmt.Errorf("unknown rejected response outcome %q", result.Outcome)
	}
}

func (r *loopRunner) rejectedResponse() (sessionstore.RejectedResponse, error) {
	rejectedReason := sessionstore.RejectedReasonUnknownFinish
	if r.lastResp.FinishType == llmwire.FinishLength {
		rejectedReason = sessionstore.RejectedReasonOutputLength
	}

	message := llmwire.Message{
		Role: llmwire.RoleAssistant, Content: r.lastResp.Text, ToolCalls: r.lastResp.ToolCalls,
		ReasoningContent: r.lastResp.ReasoningContent, ReasoningRaw: r.lastResp.ReasoningRaw,
		CostUSD: r.lastResp.CostUSD, Usage: r.lastResp.Usage, FinishType: r.lastResp.FinishType,
		ProviderFinishReason: r.lastResp.ProviderFinishReason,
	}

	stored, err := storedMessage(&message)
	if err != nil {
		return sessionstore.RejectedResponse{}, fmt.Errorf("serialize rejected response: %w", err)
	}

	stored.RejectedReason = rejectedReason

	return sessionstore.RejectedResponse{
		SessionID: r.agent.id, RootID: r.agent.rootID,
		Iteration: r.agent.iterationOffset + r.result.Iterations, Message: stored,
		ObservedAt: time.Now().UTC(),
	}, nil
}

func (r *loopRunner) persistRejectedResponse(
	ctx context.Context,
	rejection sessionstore.RejectedResponse,
) (*sessionstore.RejectedResponseResult, error) {
	if r.agent.budgetGate != nil {
		result, err := r.agent.budgetGate.PersistRejectedResponse(ctx, rejection)
		if err != nil {
			return nil, fmt.Errorf("commit budgeted rejection: %w", err)
		}

		return result, nil
	}

	store, ok := r.agent.store.(sessionstore.ResponseIntegrityStore)
	if !ok {
		return nil, errors.New("response integrity store unavailable")
	}

	result, err := store.CommitRejectedResponse(ctx, rejection)
	if err != nil {
		return nil, fmt.Errorf("commit rejection: %w", err)
	}

	return result, nil
}

func (r *loopRunner) applyRejectedOutcome(result *sessionstore.RejectedResponseResult) error {
	switch result.Outcome {
	case sessionstore.RejectedResponseRecoveryQueued:
		return r.agent.ms.adoptRecoveryMessage(result.RecoveryMessageID)
	case sessionstore.RejectedResponseBudgetSuppressed:
		r.agent.budgetFired = true
	case sessionstore.RejectedResponseRetryExhausted:
		r.setCommittedIntegrityError(sessionstore.OutputLengthTerminalError)
	case sessionstore.RejectedResponseUnknownTerminal:
		r.setCommittedIntegrityError(sessionstore.UnknownFinishTerminalError)
	default:
		return fmt.Errorf("unknown rejected response outcome %q", result.Outcome)
	}

	return nil
}

func (r *loopRunner) setCommittedIntegrityError(message string) {
	r.result.ErrorNotice = sessionstore.IntegrityErrorNotice(message)
	r.result.TerminalStateCommitted = true
	r.result.Error = errors.New(r.result.ErrorNotice)
}

func (r *loopRunner) reportRejectedIteration() error {
	if r.cb == nil {
		return nil
	}

	metadata := &llmwire.Response{
		FinishType: r.lastResp.FinishType, ProviderFinishReason: r.lastResp.ProviderFinishReason,
		CostUSD: r.lastResp.CostUSD, Usage: r.lastResp.Usage,
	}
	if err := r.cb(r.result.Iterations, metadata, nil, true); err != nil {
		return fmt.Errorf("rejected iteration callback failed: %w", err)
	}

	return nil
}

func (r *loopRunner) hasOutstandingResponseRecovery(ctx context.Context) (bool, error) {
	if r.agent.store == nil {
		return false, nil
	}

	outstanding, err := r.agent.store.HasOutstandingResponseRecovery(ctx, r.agent.id)
	if err != nil {
		return false, fmt.Errorf("load response recovery state: %w", err)
	}

	return outstanding, nil
}

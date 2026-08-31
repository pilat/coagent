package inputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var _ session.InputBoundary = (*boundary)(nil)

type boundary struct {
	store          Store
	schedules      schedule.Service
	sessionID      int64
	progress       func(context.Context) (string, error)
	progressChange func(context.Context) (string, bool, error)
	finalOutput    func(context.Context, string) (string, error)
}

func (b *boundary) FinalOutput(ctx context.Context, text string) (string, error) {
	if b.finalOutput == nil {
		return text, nil
	}

	return b.finalOutput(ctx, text)
}

func (b *boundary) ProgressChange(ctx context.Context) (string, bool, error) {
	if b.progressChange == nil {
		return "", false, errors.New("progress change provider unavailable")
	}

	return b.progressChange(ctx)
}

func (b *boundary) CurrentProgress(ctx context.Context) (string, error) {
	if b.progress == nil {
		return "", errors.New("progress provider unavailable")
	}

	return b.progress(ctx)
}

func (b *boundary) Peek(ctx context.Context) (*session.PendingInput, error) {
	input, err := b.store.PeekPending(ctx, b.sessionID)
	if errors.Is(err, sessionstore.ErrNoPendingInput) {
		return nil, nil //nolint:nilnil // nil input is the InputBoundary EOF marker.
	}

	if err != nil {
		return nil, fmt.Errorf("peek session inbox: %w", err)
	}

	return &session.PendingInput{
		ID: input.ID, Content: input.RawContent,
		Attributes: input.Attributes, ReceivedAt: input.ReceivedAt,
	}, nil
}

func (b *boundary) Accept(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
) (bool, bool, error) {
	if blockedByPendingCall(pendingCalls) {
		return false, true, nil
	}

	if len(pendingCalls) > 0 && b.schedules != nil {
		if _, err := b.schedules.CancelPendingSleeps(ctx, b.sessionID); err != nil {
			return false, false, fmt.Errorf("cancel interrupted sleep: %w", err)
		}
	}

	if _, err := b.store.PromoteInput(ctx, input.ID, prepared); err != nil {
		return false, false, fmt.Errorf("promote session input: %w", err)
	}

	return true, false, nil
}

func (b *boundary) AcceptActivated(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
	grant tool.ActivationGrant,
) (bool, bool, error) {
	if blockedByPendingCall(pendingCalls) {
		return false, true, nil
	}

	_, _, err := b.store.PromoteInputWithActivation(ctx, input.ID, prepared, sessionstore.ActivationDraft{
		ToolID: grant.ToolID, Command: grant.Command,
	})
	if err != nil {
		return false, false, fmt.Errorf("promote activated input: %w", err)
	}

	return true, false, nil
}

func (b *boundary) ExpireActivation(ctx context.Context, grant tool.ActivationGrant) error {
	_, _, err := b.store.ExpireActivationWithOutput(
		ctx, grant.InputID, grant.SessionID, "Budget was not changed",
	)
	if err != nil {
		return fmt.Errorf("expire activation with output: %w", err)
	}

	return nil
}

func (b *boundary) CancelActivation(ctx context.Context, grant tool.ActivationGrant) error {
	if _, err := b.store.ExpireActivation(ctx, grant.InputID, grant.SessionID); err != nil {
		return fmt.Errorf("cancel activation: %w", err)
	}

	return nil
}

func (b *boundary) PendingActivation(ctx context.Context) (*tool.ActivationGrant, error) {
	activation, err := b.store.CurrentActivation(ctx, b.sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil, nil //nolint:nilnil // Absence is the normal boundary state.
	}

	if err != nil {
		return nil, fmt.Errorf("load current activation: %w", err)
	}

	return &tool.ActivationGrant{
		SessionID: activation.SessionID, InputID: activation.InputID,
		ToolID: activation.ToolID, Command: activation.Command, ToolCallID: activation.ToolCallID,
	}, nil
}

func (b *boundary) Reject(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.RejectInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("reject session input: %w", err)
	}

	return nil
}

func (b *boundary) Handle(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.HandleInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("handle session input: %w", err)
	}

	return nil
}

func blockedByPendingCall(calls []session.PendingToolCall) bool {
	for _, call := range calls {
		if call.Name != tool.IDSleep {
			return true
		}
	}

	return false
}

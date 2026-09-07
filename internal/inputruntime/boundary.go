package inputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var (
	_ session.InputBoundary   = (*boundary)(nil)
	_ session.ReceiptBoundary = (*boundary)(nil)
)

type boundary struct {
	store            Store
	schedules        schedule.Service
	sessionID        int64
	progress         func(context.Context) (string, error)
	progressChange   func(context.Context) (string, bool, error)
	progressActivity func()
	finalOutput      func(context.Context, string) (string, error)
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
		ManagerOwned: managerOwnedInput(input),
	}, nil
}

func managerOwnedInput(input *sessionstore.InboxInput) bool {
	owner, _ := input.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return input.Source == sessionstore.InputSourceUser && owner != ""
}

func (b *boundary) Accept(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
) (bool, bool, error) {
	accepted, blocked, _, err := b.AcceptWithReceipt(ctx, input, prepared, pendingCalls, "")

	return accepted, blocked, err
}

// AcceptWithReceipt is Accept with one optional persistent receipt committed in
// the same transaction as the promotion. A committed receipt row returns its
// output id so the caller can notify only after the durable commit.
func (b *boundary) AcceptWithReceipt(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
	receipt string,
) (bool, bool, int64, error) {
	if blockedByPendingCall(pendingCalls) {
		return false, true, 0, nil
	}

	if len(pendingCalls) > 0 && b.schedules != nil {
		if _, err := b.schedules.CancelPendingSleeps(ctx, b.sessionID); err != nil {
			return false, false, 0, fmt.Errorf("cancel interrupted sleep: %w", err)
		}
	}

	_, commit, err := b.store.PromoteInputWithReceipt(ctx, input.ID, prepared, sessionstore.OutputDraft{
		Type:    sessionstore.OutputMessagePersistent,
		Content: receipt,
	})
	if err != nil {
		return false, false, 0, fmt.Errorf("promote session input: %w", err)
	}

	if b.progressActivity != nil {
		b.progressActivity()
	}

	var receiptID int64
	if commit != nil {
		receiptID = commit.OutputID
	}

	return true, false, receiptID, nil
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

	if b.progressActivity != nil {
		b.progressActivity()
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

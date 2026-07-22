package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type durableInputBoundary struct {
	store     sessionstore.InboxStore
	schedules interface {
		CancelPendingSleeps(context.Context, int64) (int64, error)
	}
	sessionID int64
}

var _ session.InputBoundary = (*durableInputBoundary)(nil)

func (b *durableInputBoundary) Peek(ctx context.Context) (*session.PendingInput, error) {
	input, err := b.store.PeekPending(ctx, b.sessionID)
	if errors.Is(err, sessionstore.ErrNoPendingInput) {
		//nolint:nilnil // nil input is the InputBoundary EOF marker
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("peek session inbox: %w", err)
	}

	return &session.PendingInput{
		ID:         input.ID,
		Content:    input.RawContent,
		ReceivedAt: input.ReceivedAt,
	}, nil
}

func (b *durableInputBoundary) Accept(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
) (bool, bool, error) {
	for _, call := range pendingCalls {
		if call.Name != tool.IDSleep {
			return false, true, nil
		}
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

func (b *durableInputBoundary) Reject(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.RejectInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("reject session input: %w", err)
	}

	return nil
}

func (b *durableInputBoundary) Handle(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.HandleInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("handle session input: %w", err)
	}

	return nil
}

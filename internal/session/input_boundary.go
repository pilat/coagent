package session

import (
	"context"
	"time"
)

// PendingInput is one durable normal message waiting to enter the transcript.
// ID defines FIFO order; ReceivedAt is the user-visible arrival time.
type PendingInput struct {
	ID         int64
	Content    string
	Attributes map[string]any
	ReceivedAt time.Time
}

// InputBoundary is the session-owned consumption seam for durable normal input.
// Implementations persist acceptance; wake channels are deliberately absent.
type InputBoundary interface {
	Peek(ctx context.Context) (*PendingInput, error)
	Accept(
		ctx context.Context,
		input PendingInput,
		prepared string,
		pendingCalls []PendingToolCall,
	) (accepted bool, blocked bool, err error)
	Reject(ctx context.Context, input PendingInput, reason string) error
	Handle(ctx context.Context, input PendingInput, reason string) error
}

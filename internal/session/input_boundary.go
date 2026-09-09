package session

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

// PendingInput is one durable normal message waiting to enter the transcript.
// ID defines FIFO order; ReceivedAt is the user-visible arrival time.
type PendingInput struct {
	ID           int64
	Content      string
	Attributes   map[string]any
	ReceivedAt   time.Time
	ManagerOwned bool
	Source       sessionstore.InputSource
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

// ReceiptBoundary is implemented by boundaries that can persist one
// manager-visible persistent receipt atomically with input acceptance. The
// receipt is the exact accepted-input activation marker; blocked, rejected, or
// failed promotions never reach it.
type ReceiptBoundary interface {
	AcceptWithReceipt(
		ctx context.Context,
		input PendingInput,
		prepared string,
		pendingCalls []PendingToolCall,
		receipt string,
	) (accepted bool, blocked bool, receiptID int64, err error)
}

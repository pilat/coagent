package managerdelivery

import (
	"context"
	"time"
)

const (
	SendOK SendClass = iota
	SendTransient
	SendRowPermanent
	SendChatFailure
)

type SendClass uint8

type SendResult struct {
	Class      SendClass
	Reason     string
	RetryAfter time.Duration
}

type ChatRow struct {
	ID      int64
	Name    string
	Type    string
	Content string
}

// FocusState retains an entry obligation until its cursor is acknowledged.
type FocusState struct {
	Session, Cursor int64
	Entered         bool
}

type ChatSession struct {
	ID        int64
	Name      string
	Scheduled bool
}

// ChatPort is bound to one manager and chat; session zero means no focus.
type ChatPort interface {
	Focus(context.Context) (FocusState, error)
	ResetFocus(context.Context, int64) (bool, error)
	Outputs(context.Context, int64, int64, int) ([]ChatRow, error)
	Entry(context.Context, int64, int) ([]ChatRow, int64, error)
	Enter(context.Context, int64, int64) (bool, error)
	Advance(context.Context, int64, int64) (bool, error)
	Sessions(context.Context) ([]ChatSession, error)
	Killed(context.Context, int64) (bool, error)
}

// ChatTransport owns rendering, chunking and any safe plain-text fallback.
type ChatTransport interface {
	Send(context.Context, string) SendResult
}

type ChatLoop interface {
	Start(context.Context)
	Wake()
	Stop(context.Context) error
	Focus() int64
}

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

type ChatSession struct {
	ID             int64
	Name           string
	Unread         int64
	LatestUnread   string
	LatestUnreadID int64
}

// ChatPort is bound to one manager and chat; session 0 denotes its assistant.
type ChatPort interface {
	Focus(context.Context) (int64, error)
	ResetFocus(context.Context, int64) (bool, error)
	Outputs(context.Context, int64, int) ([]ChatRow, error)
	LastSeen(context.Context, int64) (ChatRow, bool, error)
	MarkSeen(context.Context, int64, int64) error
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

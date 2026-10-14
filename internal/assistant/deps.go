package assistant

import (
	"context"
	"database/sql"
	"time"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Sessions is the assistant's manager-scoped authority over ordinary work roots.
type Sessions interface {
	Board(context.Context, time.Time) ([]Card, error)
	Read(context.Context, int64, int) (string, error)
	Open(context.Context, string, string) (Card, error)
	Send(context.Context, int64, string) error
	Stop(context.Context, int64) error
}

// Deps binds one assistant root to its durable stores and host actions.
type Deps struct {
	Store     sessionstore.AssistantInbox
	Memory    Store
	Sessions  Sessions
	NewClient func(string) (llm.Client, error)
	SayTx     func(context.Context, *sql.Tx, string) error
	Notify    func()
	Enqueue   func(context.Context, string, int64, string) error
	Now       func() time.Time
}

type loop struct {
	deps      Deps
	sessionID int64
}

type step struct {
	inputID int64
	number  int
	origin  int64
	event   Event
	desk    Desk
	board   []Card
	tape    []Line
	render  string
	raw     string
	answer  Answer
}

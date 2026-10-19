package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Sessions is the assistant's manager-scoped authority over ordinary work roots;
// every session-addressed call applies the same predicate as listings and search.
type Sessions interface {
	Recent(context.Context, int) ([]Card, int, error)
	List(context.Context, int, int, string) ([]Card, int, error)
	Projects(context.Context) ([]Project, error)
	Search(context.Context, string) ([]Hit, error)
	Around(context.Context, string, int) (string, error)
	Read(context.Context, int64, int) (string, error)
	Target(context.Context, int64) (Card, error)
	CheckProject(context.Context, string) error
	Open(context.Context, string, string, string) (Card, error)
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

// loadView gathers the desk the next turn would see for this event.
func loadView(
	ctx context.Context,
	memory Store,
	sessions Sessions,
	root int64,
	event Event,
	now time.Time,
) (View, error) {
	desk, err := memory.LoadDesk(ctx, root, event.LineID, now)
	if err != nil {
		return View{}, fmt.Errorf("read desk: %w", err)
	}

	recent, total, err := sessions.Recent(ctx, RecentSessions)
	if err != nil {
		return View{}, fmt.Errorf("read recent sessions: %w", err)
	}

	return View{Desk: desk, Recent: recent, Total: total, Event: event}, nil
}

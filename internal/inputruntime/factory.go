package inputruntime

import (
	"context"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Store owns the atomic inbox, activation, and command-output transitions used
// at the session input boundary.
type Store interface {
	sessionstore.InboxStore
	sessionstore.ActivationStore
	sessionstore.CommandOutputStore
}

// Factory creates one durable boundary per live session.
type Factory interface {
	Boundary(
		sessionID int64,
		progress func(context.Context) (string, error),
		progressChange func(context.Context) (string, bool, error),
		progressActivity func(),
		finalOutput func(context.Context, string) (string, error),
	) session.InputBoundary
}

var _ Factory = (*factory)(nil)

type factory struct {
	store     Store
	schedules schedule.Service
}

func New(store Store, schedules schedule.Service) Factory {
	return &factory{store: store, schedules: schedules}
}

func (f *factory) Boundary(
	sessionID int64,
	progress func(context.Context) (string, error),
	progressChange func(context.Context) (string, bool, error),
	progressActivity func(),
	finalOutput func(context.Context, string) (string, error),
) session.InputBoundary {
	return &boundary{
		store: f.store, schedules: f.schedules, sessionID: sessionID,
		progress: progress, progressChange: progressChange,
		progressActivity: progressActivity, finalOutput: finalOutput,
	}
}

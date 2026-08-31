package sessionlifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

var ErrShuttingDown = errors.New("session lifecycle is shutting down")

type Launcher[T any] interface {
	Ensure(
		ctx context.Context,
		sessionID int64,
		workDir string,
		projectID int64,
		inputs []T,
	) error
}

var _ Launcher[int] = (*launcher[int])(nil)

type launcher[T any] struct {
	sessions sessionstore.OrchestrationStore
	links    subagent.Store
	admit    admission.Governor
	runners  Registry[Runner[T]]

	startable  func(context.Context, *sessionstore.SessionRecord, []T) (bool, error)
	queueChild func(context.Context, int64, int64, string, int64)
	run        func(context.Context, int64, Runner[T])
}

func NewLauncher[T any](
	sessions sessionstore.OrchestrationStore,
	links subagent.Store,
	admit admission.Governor,
	runners Registry[Runner[T]],
	startable func(context.Context, *sessionstore.SessionRecord, []T) (bool, error),
	queueChild func(context.Context, int64, int64, string, int64),
	run func(context.Context, int64, Runner[T]),
) Launcher[T] {
	return &launcher[T]{
		sessions: sessions, links: links, admit: admit, runners: runners,
		startable: startable, queueChild: queueChild, run: run,
	}
}

func (l *launcher[T]) Ensure(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
	inputs []T,
) error {
	if l.runners.Closed() {
		return ErrShuttingDown
	}

	record, err := l.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session %d before start: %w", sessionID, err)
	}

	preserveStopped, err := l.startable(ctx, record, inputs)
	if err != nil {
		return err
	}

	if l.appendIfRunning(sessionID, inputs) {
		return nil
	}

	kind, parentID, blocking, err := l.slotInfo(ctx, sessionID)
	if err != nil {
		return err
	}

	if !l.admit.TryAdmit(kind, parentID) {
		if kind == admission.Child && !blocking {
			l.queueChild(ctx, sessionID, parentID, workDir, projectID)

			return nil
		}

		return admission.ErrNoCapacity
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	runner := NewRunner(cancel, workDir, projectID, kind, parentID, preserveStopped, inputs)

	existing, registered := l.runners.Register(sessionID, runner)
	if !registered {
		l.admit.Release(kind, parentID)
		cancel()

		if existing == nil {
			return ErrShuttingDown
		}

		for _, input := range inputs {
			existing.AppendInput(input)
		}

		return nil
	}

	go l.run(loopCtx, sessionID, runner) //nolint:contextcheck // Runner lifetime must outlive the request.

	return nil
}

func (l *launcher[T]) appendIfRunning(sessionID int64, inputs []T) bool {
	return l.runners.Use(sessionID, func(existing Runner[T]) {
		for _, input := range inputs {
			existing.AppendInput(input)
		}
	})
}

func (l *launcher[T]) slotInfo(
	ctx context.Context,
	sessionID int64,
) (admission.Kind, int64, bool, error) {
	link, err := l.links.GetLink(ctx, sessionID)
	if err != nil {
		return admission.Parent, 0, false, fmt.Errorf("classify session %d: %w", sessionID, err)
	}

	if link == nil {
		return admission.Parent, 0, false, nil
	}

	return admission.Child, link.ParentID, link.Blocking, nil
}

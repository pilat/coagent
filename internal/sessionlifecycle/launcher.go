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

type Launcher interface {
	Ensure(
		ctx context.Context,
		sessionID int64,
		workDir string,
		projectID int64,
	) error
}

var _ Launcher = (*launcher)(nil)

type launcher struct {
	sessions sessionstore.OrchestrationStore
	links    subagent.Store
	admit    admission.Governor
	runners  Registry[Runner]

	startable  func(context.Context, *sessionstore.SessionRecord) (bool, error)
	queueChild func(context.Context, int64, int64, string, int64)
	run        func(context.Context, int64, Runner)
}

func NewLauncher(
	sessions sessionstore.OrchestrationStore,
	links subagent.Store,
	admit admission.Governor,
	runners Registry[Runner],
	startable func(context.Context, *sessionstore.SessionRecord) (bool, error),
	queueChild func(context.Context, int64, int64, string, int64),
	run func(context.Context, int64, Runner),
) Launcher {
	return &launcher{
		sessions: sessions, links: links, admit: admit, runners: runners,
		startable: startable, queueChild: queueChild, run: run,
	}
}

func (l *launcher) Ensure(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
) error {
	if l.runners.Closed() {
		return ErrShuttingDown
	}

	record, err := l.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session %d before start: %w", sessionID, err)
	}

	preserveStopped, err := l.startable(ctx, record)
	if err != nil {
		return err
	}

	if l.appendIfRunning(sessionID) {
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
	runner := NewRunner(cancel, workDir, projectID, kind, parentID, preserveStopped)

	existing, registered := l.runners.Register(sessionID, runner)
	if !registered {
		l.admit.Release(kind, parentID)
		cancel()

		if existing == nil {
			return ErrShuttingDown
		}

		return nil
	}

	go l.run(loopCtx, sessionID, runner) //nolint:contextcheck // Runner lifetime must outlive the request.

	return nil
}

func (l *launcher) appendIfRunning(sessionID int64) bool {
	_, running := l.runners.Load(sessionID)
	return running
}

func (l *launcher) slotInfo(
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

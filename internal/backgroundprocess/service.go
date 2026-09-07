package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const (
	// LiveProcessLimit bounds concurrent processes owned by one session.
	LiveProcessLimit = 4
	// MaxOutputBytes is the per-process output file limit.
	MaxOutputBytes = 100 * 1024 * 1024
	// TailPreviewLines bounds completion previews.
	TailPreviewLines = 50
	// TailPreviewBytes bounds completion previews after line selection.
	TailPreviewBytes = 8 * 1024
	joinGrace        = 10 * time.Second
	drainTimeout     = 5 * time.Second
	outputSizeFlush  = 2 * time.Second
)

var (
	// ErrSlotLimit reports that a session has exhausted its process slots.
	ErrSlotLimit = errors.New("process slot limit reached for session")
	// ErrFenced reports that the session tree no longer accepts processes.
	ErrFenced = errors.New("session tree is being stopped")
)

// Spec describes process ownership and execution limits.
type Spec struct {
	SessionID     int64
	RootSessionID int64
	ToolCallID    string
	Deadline      time.Duration
	Advertise     bool
}

// Completion is the durable terminal process notification.
type Completion struct {
	ProcessID   string
	SessionID   int64
	RootID      int64
	ToolCallID  string
	State       State
	ExitCode    int
	HasExitCode bool
	Duration    time.Duration
	OutputPath  string
	OutputSize  int64
	Tail        string
	TailOmitted bool
	BinaryTail  bool
}

// OnCompletion receives an advertised process completion.
type OnCompletion func(ctx context.Context, completion Completion)

// TreeFence holds process admission against a root-tree stop transition.
type TreeFence func(ctx context.Context, rootSessionID int64) (release func(), err error)

// Options configures process storage, delivery, and admission.
type Options struct {
	OutputDir    string
	OnCompletion OnCompletion
	TreeFence    TreeFence
	Now          func() time.Time
}

// Service owns process execution and durable lifecycle transitions.
type Service interface {
	Store() Store
	RemoveOutput(ctx context.Context, processID string) error
	Start(ctx context.Context, spec Spec, spawn func(context.Context) (*exec.Cmd, error)) (Process, error)
	Advertise(ctx context.Context, processID string) (bool, error)
	CancelProcess(ctx context.Context, processID string, intent HostIntent) (int, error)
	CancelTree(ctx context.Context, rootSessionID int64, intent HostIntent) (int, error)
	CancelSessions(ctx context.Context, sessionIDs []int64, intent HostIntent) (int, error)
	CancelAll(ctx context.Context, intent HostIntent) (int, error)
	InterruptNonterminal(ctx context.Context) (int, error)
}

var _ Service = (*svc)(nil)

type svc struct {
	store           Store
	opts            Options
	mu              sync.Mutex
	live            map[int64]int
	cancels         map[string]context.CancelFunc
	liveRecords     map[string]Process
	fallbackIntents map[string]HostIntent
	ids             int64
	closed          bool
}

type launchResult struct {
	cmd        *exec.Cmd
	collector  *collector
	record     Process
	quotaReady chan<- bool
	cancel     context.CancelFunc
}

// NewService constructs a process lifecycle service.
func NewService(store Store, opts Options) Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &svc{
		store: store, opts: opts, live: make(map[int64]int),
		cancels: make(map[string]context.CancelFunc), liveRecords: make(map[string]Process),
		fallbackIntents: make(map[string]HostIntent),
	}
}

func (s *svc) Store() Store {
	return s.store
}

func (s *svc) RemoveOutput(ctx context.Context, processID string) error {
	record, err := s.store.GetProcess(ctx, processID)
	if err != nil {
		return fmt.Errorf("resolve output file: %w", err)
	}

	if record.AdvertisedAt != nil {
		return nil
	}

	if err := os.Remove(record.OutputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove candidate output file: %w", err)
	}

	return nil
}

func (s *svc) Start(
	ctx context.Context,
	spec Spec,
	spawn func(ctx context.Context) (*exec.Cmd, error),
) (Process, error) {
	if s.opts.OutputDir == "" {
		return Process{}, errors.New("process output directory is unavailable")
	}

	if s.opts.TreeFence != nil {
		releaseFence, err := s.opts.TreeFence(ctx, spec.RootSessionID)
		if err != nil {
			return Process{}, err
		}
		defer releaseFence()
	}

	if err := s.reserve(spec.SessionID); err != nil {
		return Process{}, err
	}

	launched, err := s.launch(ctx, spec, spawn)
	if err != nil {
		s.release(spec.SessionID)

		return Process{}, err
	}

	s.track(launched.record, launched.cancel)

	if err := s.store.InsertProcess(ctx, launched.record); err != nil {
		return Process{}, s.abortUnpersisted(spec.SessionID, launched, err)
	}

	launched.quotaReady <- true

	closed := s.admissionClosed()
	if closed {
		if _, err := s.store.RecordIntent(
			context.WithoutCancel(ctx), launched.record.ID, IntentDaemonShutdown,
		); err != nil {
			s.recordFallbackIntent(launched.record.ID, IntentDaemonShutdown)
		}

		launched.cancel()
	}

	s.startSupervisor(context.WithoutCancel(ctx), launched)

	if closed {
		return Process{}, ErrFenced
	}

	return launched.record, nil
}

func (s *svc) Advertise(ctx context.Context, processID string) (bool, error) {
	advertised, err := s.store.Advertise(ctx, processID, s.opts.Now())
	if err != nil {
		return false, fmt.Errorf("advertise process: %w", err)
	}

	return advertised, nil
}

func (s *svc) abortUnpersisted(sessionID int64, launched *launchResult, insertErr error) error {
	_ = killGroup(launched.cmd)
	launched.quotaReady <- false

	_ = launched.cmd.Wait()
	_ = launched.collector.Close()
	s.untrack(launched.record.ID)
	launched.cancel()
	s.release(sessionID)

	return fmt.Errorf("persist background process: %w", insertErr)
}

func (s *svc) startSupervisor(ctx context.Context, launched *launchResult) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(ctx).Named("backgroundprocess.supervise").Error(
					"panic", zap.Any("recovered", recovered), zap.Stack("stack"),
				)
			}
		}()

		s.supervise(ctx, launched)
	}()
}

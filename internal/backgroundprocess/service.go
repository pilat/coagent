package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const (
	// LiveProcessLimit is the per-exact-session concurrent process cap.
	LiveProcessLimit = 4

	// MaxOutputBytes is the per-process combined stdout/stderr cap.
	MaxOutputBytes = 100 * 1024 * 1024

	// TailPreviewLines bounds the completion-event preview.
	TailPreviewLines = 50

	// TailPreviewBytes bounds the completion-event preview.
	TailPreviewBytes = 8 * 1024

	// joinGrace bounds how long CancelTree waits for terminalization.
	joinGrace = 10 * time.Second

	// drainTimeout marks a process whose pipes never closed after exit.
	drainTimeout = 5 * time.Second
)

// Errors returned by Start.
var (
	ErrSlotLimit = errors.New("process slot limit reached for session")
	ErrFenced    = errors.New("session tree is being stopped")
)

// Spec describes a process to launch on behalf of a session.
type Spec struct {
	SessionID     int64
	RootSessionID int64
	ToolCallID    string
	Deadline      time.Duration
	Advertise     bool
}

// Completion is the bounded host-owned terminal fact.
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

// OnCompletion receives exactly one fact per terminalized process.
type OnCompletion func(ctx context.Context, completion Completion)

// TreeFence returns non-nil when the root tree may no longer start processes.
type TreeFence func(rootSessionID int64) error

type Options struct {
	OutputDir    string
	OnCompletion OnCompletion
	TreeFence    TreeFence
	Now          func() time.Time
}

// Service owns process identity, per-session admission, the combined
// collector, and terminalization. OS PIDs never leave this package as
// recovery handles.
type Service struct {
	store   Store
	opts    Options
	mu      sync.Mutex
	live    map[int64]int
	cancels map[string]context.CancelFunc
	ids     int64
}

// NewService builds the process lifecycle service.
func NewService(store Store, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Service{
		store:   store,
		opts:    opts,
		live:    make(map[int64]int),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Store exposes the ledger for owner-side foreground polling. Lifecycle
// transitions stay inside the service; callers must not terminalize.

func (s *Service) Store() Store {
	return s.store
}

// RemoveOutput deletes an unadvertised candidate output file after an inline
// result consumed it. Advertised output files are kept.
func (s *Service) RemoveOutput(ctx context.Context, processID string) error {
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

// Start reserves a slot, spawns the command, and persists the ledger row.
// spawn receives the process lifetime context owned by this service, not the
// caller's session context. The returned record exists durably before the
// first output byte.

func (s *Service) Start(
	ctx context.Context,
	spec Spec,
	spawn func(ctx context.Context) (*exec.Cmd, error),
) (Process, error) {
	if s.opts.TreeFence != nil {
		if err := s.opts.TreeFence(spec.RootSessionID); err != nil {
			return Process{}, ErrFenced
		}
	}

	if err := s.reserve(spec.SessionID); err != nil {
		return Process{}, err
	}

	cmd, collector, record, cancel, err := s.launch(ctx, spec, spawn)
	if err != nil {
		if cancel != nil {
			cancel()
		}

		s.release(spec.SessionID)

		return Process{}, err
	}

	if err := s.store.InsertProcess(ctx, record); err != nil {
		_ = killGroup(cmd)
		_, _ = cmd.Process.Wait()
		_ = collector.Close()

		return Process{}, fmt.Errorf("persist background process: %w", err)
	}

	s.track(record.ID, cancel)

	go s.supervise(ctx, cmd, collector, record)

	return record, nil
}

// CancelTree records the operator intent for every running process owned by
// the root tree, signals the in-memory handles, and joins terminalization.
// Stored OS PIDs are never re-signalled after restart.

func (s *Service) CancelTree(
	ctx context.Context,
	rootSessionID int64,
	intent HostIntent,
) (int, error) {
	running, err := s.store.ListRunningByRoot(ctx, rootSessionID)
	if err != nil {
		return 0, fmt.Errorf("list tree processes: %w", err)
	}

	return s.cancelRecords(ctx, running, intent)
}

// CancelAll records the given intent for every running process regardless of
// owner, signals in-memory handles, and joins terminalization. Used by
// controlled daemon shutdown; wake events stay owned by the caller.

func (s *Service) CancelAll(ctx context.Context, intent HostIntent) (int, error) {
	running, err := s.store.ListRunning(ctx)
	if err != nil {
		return 0, fmt.Errorf("list running processes: %w", err)
	}

	return s.cancelRecords(ctx, running, intent)
}

// InterruptNonterminal startup sweep: durably terminalize every leftover
// advertised running record as interrupted without signalling stored PIDs.

func (s *Service) InterruptNonterminal(ctx context.Context) (int, error) {
	running, err := s.store.ListRunningAdvertised(ctx)
	if err != nil {
		return 0, fmt.Errorf("list leftover processes: %w", err)
	}

	interrupted := 0

	for _, process := range running {
		_, ok, err := s.store.Finalize(ctx, process.ID, StateInterrupted, nil)
		if err != nil {
			return interrupted, fmt.Errorf("interrupt process %s: %w", process.ID, err)
		}

		if !ok {
			continue
		}

		interrupted++

		// A live handle in this daemon still exists when the sweep races a
		// process; cancel it so the slot joins instead of waiting out the
		// deadline. The supervise goroutine loses the Finalize CAS and
		// emits nothing.
		s.mu.Lock()
		cancel := s.cancels[process.ID]
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}
	}

	return interrupted, nil
}

// launch builds the command, output file, and ledger record. On error the
// caller still owns the reservation release.

func (s *Service) launch(
	ctx context.Context,
	spec Spec,
	spawn func(ctx context.Context) (*exec.Cmd, error),
) (*exec.Cmd, *collector, Process, context.CancelFunc, error) {
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	cmd, err := spawn(processCtx)
	if err != nil {
		return nil, nil, Process{}, cancel, fmt.Errorf("spawn background process: %w", err)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("cancel process group: %w", err)
		}

		return nil
	}
	cmd.WaitDelay = drainTimeout

	processID := s.newProcessID()

	outputDir := filepath.Join(s.opts.OutputDir, strconv.FormatInt(spec.SessionID, 10))
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, nil, Process{}, cancel, fmt.Errorf("create process output dir: %w", err)
	}

	outputPath := filepath.Join(outputDir, processID+".output")

	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, Process{}, cancel, fmt.Errorf("create process output file: %w", err)
	}

	now := s.opts.Now()
	record := Process{
		ID:            processID,
		SessionID:     spec.SessionID,
		RootSessionID: spec.RootSessionID,
		ToolCallID:    spec.ToolCallID,
		OutputPath:    outputPath,
		Deadline:      now.Add(spec.Deadline),
		CreatedAt:     now,
		State:         StateRunning,
	}

	if spec.Advertise {
		advertised := now
		record.AdvertisedAt = &advertised
	}

	collector := newCollector(file, MaxOutputBytes, func() {
		_, _ = s.store.RecordIntent(processCtx, processID, IntentOutputLimit)

		go func() {
			_ = killGroup(cmd)
		}()
	})

	cmd.Stdout = collector
	cmd.Stderr = collector

	if err := cmd.Start(); err != nil {
		_ = collector.Close()

		return nil, nil, Process{}, cancel, fmt.Errorf("start background process: %w", err)
	}

	return cmd, collector, record, cancel, nil
}

func (s *Service) newProcessID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ids++
	// Opaque identity; never an OS pid.
	return fmt.Sprintf("bgp_%d_%d", s.opts.Now().UnixNano(), s.ids)
}

func (s *Service) reserve(sessionID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.live[sessionID] >= LiveProcessLimit {
		return ErrSlotLimit
	}

	s.live[sessionID]++

	return nil
}

func (s *Service) release(sessionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.live[sessionID] > 0 {
		s.live[sessionID]--
	}
}

func (s *Service) liveCount(sessionID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.live[sessionID]
}

func (s *Service) track(processID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cancels[processID] = cancel
}

func (s *Service) untrack(processID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.cancels, processID)
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}

// supervise races cmd.Wait against the deadline and terminalizes exactly once.

func (s *Service) supervise(
	ctx context.Context,
	cmd *exec.Cmd,
	collector *collector,
	record Process,
) {
	defer s.release(record.SessionID)
	defer s.untrack(record.ID)

	timer := time.NewTimer(time.Until(record.Deadline))
	defer timer.Stop()

	waitErr := make(chan error, 1)

	go func() { waitErr <- cmd.Wait() }()

	var natural State
	var exitCode *int

	select {
	case err := <-waitErr:
		natural, exitCode = classifyExit(err)
	case <-timer.C:
		_, _ = s.store.RecordIntent(ctx, record.ID, IntentDeadline)
		_ = killGroup(cmd)

		<-waitErr

		natural = StateTimedOut
	}

	// Join the collector after the process exits; the WaitDelay bound above
	// keeps a stuck descriptor from blocking forever, surfacing as
	// output_drain_timeout below.
	if err := collector.Close(); err != nil {
		natural = StateOutputDrainTimeout
		exitCode = nil
	}

	if err := s.store.UpdateOutputSize(ctx, record.ID, collector.Size()); err != nil {
		logger.Ctx(ctx).Named("backgroundprocess").Warn("output_size_update_failed", zap.Error(err))
	}

	finalized, won, err := s.store.Finalize(ctx, record.ID, natural, exitCode)
	if err != nil {
		return
	}

	if won {
		record = finalized
	}

	s.emit(ctx, record, won)
}

func classifyExit(err error) (State, *int) {
	code := 0
	if err == nil {
		return StateCompleted, &code
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		failed := exitErr.ExitCode()

		return StateFailed, &failed
	}

	return StateFailed, nil
}

// emit delivers the bounded completion fact once per terminalized process.
// Session stop/kill suppresses individual wake events, and shutdown
// interruption leaves delivery owed to the next startup sweep.

func (s *Service) emit(ctx context.Context, record Process, won bool) {
	if s.opts.OnCompletion == nil || !won {
		return
	}

	if record.WakeSuppressed() || record.State == StateInterrupted {
		return
	}

	duration := record.FinishedAt.Sub(record.CreatedAt)
	tail, ok := ExtractTail(record.OutputPath, TailPreviewLines, TailPreviewBytes)
	tailOmitted := !ok

	if ok && int64(len(tail)) > TailPreviewBytes {
		tail, _ = truncateTailBytes(tail, TailPreviewBytes)
	}

	completion := Completion{
		ProcessID:   record.ID,
		SessionID:   record.SessionID,
		RootID:      record.RootSessionID,
		ToolCallID:  record.ToolCallID,
		State:       record.State,
		Duration:    duration,
		OutputPath:  record.OutputPath,
		OutputSize:  record.OutputSize,
		Tail:        tail,
		TailOmitted: tailOmitted,
		BinaryTail:  tailOmitted,
	}

	if record.ExitCode != nil {
		completion.ExitCode = *record.ExitCode
		completion.HasExitCode = true
	}

	s.opts.OnCompletion(ctx, completion)
}

func (s *Service) cancelRecords(
	ctx context.Context,
	running []Process,
	intent HostIntent,
) (int, error) {
	cancelled := 0

	for _, process := range running {
		won, err := s.store.RecordIntent(ctx, process.ID, intent)
		if err != nil {
			return cancelled, fmt.Errorf("record cancel intent for %s: %w", process.ID, err)
		}

		if !won {
			continue
		}

		cancelled++

		s.mu.Lock()
		cancel := s.cancels[process.ID]
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}
	}

	if cancelled == 0 {
		return 0, nil
	}

	deadline := time.Now().Add(joinGrace)

	for time.Now().Before(deadline) {
		still, err := s.store.ListRunning(ctx)
		if err != nil {
			return cancelled, fmt.Errorf("join cancelled processes: %w", err)
		}

		if len(still) == 0 {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	return cancelled, nil
}

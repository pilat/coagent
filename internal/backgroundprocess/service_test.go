package backgroundprocess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := migrate.OpenDB(context.Background(),
		filepath.Join(t.TempDir(), "bg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func newTestStore(t *testing.T) Store {
	t.Helper()

	ctx := context.Background()
	db := newTestDB(t)

	if err := migrate.Run(ctx, db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	for _, id := range []int{2, 3} {
		_, err = db.ExecContext(ctx, `
			INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id)
			VALUES (?, 1, 'm', 'general', 1, 1)`, id)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (4, 1, 'm', 'build')`)
	require.NoError(t, err)

	return NewStore(db)
}

func testSpec(sessionID int64) Spec {
	return Spec{
		ProjectDir:    "project-1",
		SessionID:     sessionID,
		RootSessionID: 1,
		ToolCallID:    "call_1",
		Deadline:      30 * time.Second,
		Advertise:     true,
	}
}

func runningRecord(t *testing.T, store Store, sessionID int64) Process {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Second)
	record := Process{
		ID:            newTestID(),
		SessionID:     sessionID,
		RootSessionID: 1,
		ToolCallID:    "call_1",
		OutputPath:    filepath.Join(t.TempDir(), "out.output"),
		Deadline:      now.Add(time.Minute),
		CreatedAt:     now,
		AdvertisedAt:  &now,
		State:         StateRunning,
	}

	require.NoError(t, store.InsertProcess(context.Background(), record))

	return record
}

var testIDCounter int

type blockingInsertStore struct {
	Store
	reached          chan Process
	proceed          chan struct{}
	failRecordIntent bool
}

func (s *blockingInsertStore) InsertProcess(ctx context.Context, process Process) error {
	s.reached <- process
	<-s.proceed

	return s.Store.InsertProcess(ctx, process)
}

func (s *blockingInsertStore) RecordIntent(
	ctx context.Context,
	id string,
	intent HostIntent,
) (bool, error) {
	if s.failRecordIntent {
		return false, errors.New("injected shutdown intent failure")
	}

	return s.Store.RecordIntent(ctx, id, intent)
}

func newTestID() string {
	testIDCounter++

	return "bgp_test_" + time.Now().Format("150405.000000000") + "_" +
		string(rune('a'+testIDCounter%26))
}

func TestStore_FinalizeIntentPrecedence(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	record := runningRecord(t, store, 2)

	won, err := store.RecordIntent(ctx, record.ID, IntentDeadline)
	require.NoError(t, err)
	require.True(t, won)

	// Natural completion arrives after the deadline intent: intent wins.
	finalized, won, err := store.Finalize(ctx, record.ID, StateCompleted, nil, 12)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, StateTimedOut, finalized.State)
	assert.Nil(t, finalized.ExitCode)
	assert.NotNil(t, finalized.FinishedAt)
	assert.Equal(t, int64(12), finalized.OutputSize)

	// A second finalize loses and re-reads the winner.
	loser, won, err := store.Finalize(ctx, record.ID, StateCompleted, nil, 99)
	require.NoError(t, err)
	assert.False(t, won)
	assert.Equal(t, StateTimedOut, loser.State)
}

func TestStore_FinalizeNaturalExitWithoutIntent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	record := runningRecord(t, store, 2)
	code := 7

	finalized, won, err := store.Finalize(ctx, record.ID, StateFailed, &code, 42)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, StateFailed, finalized.State)
	require.NotNil(t, finalized.ExitCode)
	assert.Equal(t, 7, *finalized.ExitCode)
}

func TestStore_FinalizeInsertsProcessInboxInput(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	require.NoError(t, migrate.Run(ctx, db, ""))
	require.NoError(t, seedProcessTestSessions(ctx, db))
	store := NewStore(db)
	record := runningRecord(t, store, 2)
	zero := 0
	_, won, err := store.Finalize(ctx, record.ID, StateCompleted, &zero, 0)
	require.NoError(t, err)
	require.True(t, won)

	var (
		source         string
		inputSessionID int64
		content        string
	)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT session_id, source, raw_content
		FROM session_inbox WHERE json_extract(attributes, '$.process_id') = ?`, record.ID).
		Scan(&inputSessionID, &source, &content))
	assert.Equal(t, record.SessionID, inputSessionID)
	assert.Equal(t, "process", source)
	assert.Contains(t, content, "<process_completion>")
	assert.Contains(t, content, "process_id: "+record.ID)
}

func TestStore_FinalizeRetainsFactsBySessionStatus(t *testing.T) {
	tests := []struct {
		status string
		killed bool
		want   int
	}{
		{status: "active", want: 1},
		{status: "suspended", want: 1},
		{status: "completed", want: 1},
		{status: "stopped", want: 1},
		{status: "error", want: 1},
		{status: "stopping", want: 1},
		{status: "terminating"},
		{status: "killed", killed: true},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			ctx := context.Background()
			db := newTestDB(t)
			require.NoError(t, migrate.Run(ctx, db, ""))
			require.NoError(t, seedProcessTestSessions(ctx, db))
			var killedAt any
			if tt.killed {
				killedAt = time.Now().UTC()
			}
			_, err := db.ExecContext(ctx, `UPDATE sessions SET status = ?, killed_at = ? WHERE id = 2`,
				tt.status, killedAt)
			require.NoError(t, err)

			ledger := NewStore(db)
			record := runningRecord(t, ledger, 2)
			zero := 0
			_, won, err := ledger.Finalize(ctx, record.ID, StateCompleted, &zero, 0)
			require.NoError(t, err)
			require.True(t, won)

			var count int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_inbox
				WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, record.ID).
				Scan(&count))
			assert.Equal(t, tt.want, count)
		})
	}
}

func TestFormatCompletionEscapesDynamicValues(t *testing.T) {
	created := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	finished := created.Add(1500 * time.Millisecond)
	output := filepath.Join(t.TempDir(), "result.output")
	require.NoError(t, os.WriteFile(output, []byte("<done>&\n"), 0o600))
	code := 0

	content := formatCompletion(Process{
		ID: "bgp_<one>", SessionID: 7, State: StateCompleted, ExitCode: &code,
		OutputPath: output, CreatedAt: created, FinishedAt: &finished,
	})
	assert.Equal(t, strings.Join([]string{
		"<process_completion>",
		"process_id: bgp_&lt;one&gt;",
		"state: completed",
		"exit_code: 0",
		"duration: 1.5s",
		"origin_session_id: 7",
		"output_file: " + output,
		"preview_status: text",
		"final_output_preview:",
		"&lt;done&gt;&amp;",
		"</process_completion>",
	}, "\n"), content)
}

func seedProcessTestSessions(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`,
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (1, 1, 'm', 'build')`,
	); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, parent_id, root_id, model, agent_type)
		VALUES (2, 1, 1, 1, 'm', 'general')`)
	return err
}

func TestStore_ListQueries(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	root := runningRecord(t, store, 1)
	other := runningRecord(t, store, 2)
	other.RootSessionID = 1
	require.NoError(t, store.UpdateOutputSize(ctx, other.ID, 42))

	byRoot, err := store.ListRunningByRoot(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, byRoot, 2)

	byRoot2, err := store.ListRunningByRoot(ctx, 1)
	require.NoError(t, err)
	var matched *Process

	for i := range byRoot2 {
		if byRoot2[i].ID == other.ID {
			matched = &byRoot2[i]
		}
	}

	require.NotNil(t, matched)
	assert.Equal(t, int64(42), matched.OutputSize)

	advertised, err := store.ListRunningAdvertised(ctx)
	require.NoError(t, err)
	assert.Len(t, advertised, 2)

	zero := 0
	_, won, err := store.Finalize(ctx, root.ID, StateCompleted, &zero, 0)
	require.NoError(t, err)
	require.True(t, won)

	byRoot, err = store.ListRunningByRoot(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, byRoot, 1)
}

// waitState polls the ledger until the process reaches the state.
func waitState(
	t *testing.T,
	store Store,
	id string,
	state State,
	timeout time.Duration,
) Process {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		record, err := store.GetProcess(context.Background(), id)
		require.NoError(t, err)

		if record.State == state {
			return record
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("process %q did not reach state %q within %s", id, state, timeout)

	return Process{}
}

func newTestService(
	t *testing.T,
	store Store,
	onCompletion func(context.Context, Completion),
	fence TreeFence,
) *svc {
	t.Helper()

	return NewService(store, Options{
		OutputDir:       t.TempDir(),
		OnCompletion:    onCompletion,
		TreeFence:       fence,
		Now:             func() time.Time { return time.Now().UTC() },
		GuardianCommand: testGuardianCommand,
	}).(*svc)
}

func TestProcessGuardianHelper(t *testing.T) {
	if os.Getenv("COAGENT_TEST_PROCESS_GUARDIAN") != "1" {
		return
	}

	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "guardian helper separator is missing")
		os.Exit(1)
	}

	_, err := RunGuardian(os.Args[separator+1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestService_SlotLimitPerSession(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	spawn := func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}

	for range BackgroundProcessLimit {
		_, err := service.Start(ctx, testSpec(2), spawn)
		require.NoError(t, err)
	}

	_, err := service.Start(ctx, testSpec(2), spawn)
	require.ErrorIs(t, err, ErrSlotLimit)

	// A different exact session owns an independent allowance.
	for range BackgroundProcessLimit {
		_, err := service.Start(ctx, testSpec(3), spawn)
		require.NoError(t, err)
	}

	_, err = service.Start(ctx, testSpec(3), spawn)
	require.ErrorIs(t, err, ErrSlotLimit)

	cancelled, err := service.CancelTree(ctx, 1, IntentSessionKilled)
	require.NoError(t, err)
	assert.Equal(t, 8, cancelled)

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0 && service.liveCount(3) == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestService_AdmissionSeparatesCandidateAndBackgroundCapacity(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)
	spawn := func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}

	for range BackgroundProcessLimit {
		_, err := service.Start(ctx, testSpec(2), spawn)
		require.NoError(t, err)
	}

	candidateSpec := testSpec(2)
	candidateSpec.Advertise = false
	candidate, err := service.Start(ctx, candidateSpec, spawn)
	require.NoError(t, err)
	_, err = service.Start(ctx, candidateSpec, spawn)
	require.ErrorIs(t, err, ErrCandidateLimit)

	advertised, err := service.Advertise(ctx, candidate.ID)
	require.ErrorIs(t, err, ErrSlotLimit)
	assert.False(t, advertised)
	stored, err := store.GetProcess(ctx, candidate.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.AdvertisedAt)

	_, err = service.CancelTree(ctx, 1, IntentSessionKilled)
	require.NoError(t, err)
}

func TestService_AdvertisePromotesCandidateAdmissionClass(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)
	spawn := func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}

	for range BackgroundProcessLimit - 1 {
		_, err := service.Start(ctx, testSpec(2), spawn)
		require.NoError(t, err)
	}
	candidateSpec := testSpec(2)
	candidateSpec.Advertise = false
	candidate, err := service.Start(ctx, candidateSpec, spawn)
	require.NoError(t, err)

	advertised, err := service.Advertise(ctx, candidate.ID)
	require.NoError(t, err)
	assert.True(t, advertised)
	service.mu.Lock()
	assert.Equal(t, BackgroundProcessLimit, service.background[2])
	assert.Zero(t, service.candidates[2])
	assert.Equal(t, admissionBackground, service.classes[candidate.ID])
	service.mu.Unlock()

	_, err = service.CancelTree(ctx, 1, IntentSessionKilled)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return service.liveCount(2) == 0 }, 10*time.Second, 20*time.Millisecond)
}

func TestService_ForegroundCompletion(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	var mu sync.Mutex
	var completions []Completion
	service := newTestService(t, store, func(_ context.Context, completion Completion) {
		mu.Lock()
		defer mu.Unlock()

		completions = append(completions, completion)
	}, nil)

	record, err := service.Start(ctx, testSpec(2),
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sh", "-c",
				"echo hello; echo oops >&2"), nil
		})
	require.NoError(t, err)
	assert.NotEmpty(t, record.ID)
	assert.Contains(t, record.OutputPath, filepath.Join("project-1", "2"))

	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(completions) == 1
	}, 10*time.Second, 20*time.Millisecond)

	mu.Lock()
	completion := completions[0]
	mu.Unlock()

	assert.Equal(t, record.ID, completion.ProcessID)
	assert.Equal(t, StateCompleted, completion.State)
	assert.Equal(t, int64(2), completion.SessionID)
	assert.Equal(t, int64(1), completion.RootID)
	assert.Contains(t, completion.Tail, "hello")
	assert.Contains(t, completion.Tail, "oops")

	final, err := store.GetProcess(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, StateCompleted, final.State)

	data, err := os.ReadFile(record.OutputPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "hello")

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestService_CompletionPanicDoesNotEscapeSupervisor(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, func(context.Context, Completion) {
		panic("completion callback")
	}, nil)

	record, err := service.Start(ctx, testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", "printf done"), nil
	})
	require.NoError(t, err)

	waitState(t, store, record.ID, StateCompleted, 5*time.Second)
	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 5*time.Second, 20*time.Millisecond)
}

func TestService_OutputLimitKillsGroupAndKeepsDraining(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	record, err := service.Start(ctx, testSpec(2),
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "yes", "0123456789abcdef"), nil
		})
	require.NoError(t, err)

	final := waitState(t, store, record.ID, StateOutputLimitExceeded, 60*time.Second)
	assert.NotNil(t, final.FinishedAt)

	data, err := os.ReadFile(record.OutputPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), overflowMarker)
	assert.LessOrEqual(t, len(data), int(MaxOutputBytes)+len(overflowMarker))

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestService_DeadlineKillsGroup(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	spec := testSpec(2)
	spec.Deadline = 300 * time.Millisecond

	record, err := service.Start(ctx, spec,
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	final := waitState(t, store, record.ID, StateTimedOut, 15*time.Second)
	assert.Nil(t, final.ExitCode, "a group-killed deadline has no meaningful exit code")

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestService_CancelTreeSuppressesAndCounts(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	var mu sync.Mutex
	var completions []Completion
	service := newTestService(t, store, func(_ context.Context, completion Completion) {
		mu.Lock()
		defer mu.Unlock()

		completions = append(completions, completion)
	}, nil)

	_, err := service.Start(ctx, testSpec(2),
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	// A process owned outside the tree is untouched by this stop.
	outsideSpec := testSpec(4)
	outsideSpec.RootSessionID = 4
	_, err = service.Start(ctx, outsideSpec,
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	cancelled, err := service.CancelTree(ctx, 1, IntentSessionStopped)
	require.NoError(t, err)
	assert.Equal(t, 1, cancelled)

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 10*time.Second, 20*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(completions)
	mu.Unlock()
	assert.Equal(t, 0, count, "cancellation must suppress wake events")

	// Cleanup for the outside-tree process.
	_, err = service.CancelTree(ctx, 4, IntentSessionKilled)
	require.NoError(t, err)
}

func TestService_TreeFenceRejectsStart(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	service := newTestService(t, store, nil, func(context.Context, int64) (func(), error) {
		return nil, ErrFenced
	})

	_, err := service.Start(ctx, testSpec(2),
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.ErrorIs(t, err, ErrFenced)
	assert.Equal(t, 0, service.liveCount(2))
}

func TestService_GuardianExecFailureStartsNoCommand(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	marker := filepath.Join(t.TempDir(), "started")
	service := NewService(store, Options{
		OutputDir: t.TempDir(),
		GuardianCommand: func(string, *os.File, *os.File) *exec.Cmd {
			return exec.Command(filepath.Join(t.TempDir(), "missing-guardian"))
		},
	})

	_, err := service.Start(ctx, testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", "touch "+marker), nil
	})
	require.ErrorContains(t, err, "start process guardian")
	assert.NoFileExists(t, marker)

	running, err := store.ListRunning(ctx)
	require.NoError(t, err)
	assert.Empty(t, running)
}

func TestService_InterruptNonterminalSweepsAdvertised(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	spec := testSpec(2)

	record, err := service.Start(ctx, spec,
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	count, err := service.InterruptNonterminal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	final := waitState(t, store, record.ID, StateInterrupted, 5*time.Second)
	assert.Nil(t, final.ExitCode, "an interrupted sweep never signals a stored PID")

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestCollector_DiscardAfterCap(t *testing.T) {
	dir := t.TempDir()

	file, err := os.OpenFile(
		filepath.Join(dir, "out.output"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)

	capped := make(chan struct{}, 1)
	collector := newCollector(file, 16, func() {
		capped <- struct{}{}
	})

	_, err = collector.Write([]byte("0123456789"))
	require.NoError(t, err)

	// Crossing the cap does not error and keeps consuming.
	n, err := collector.Write([]byte("ABCDEFGHIJ"))
	require.NoError(t, err)
	assert.Equal(t, 10, n)

	select {
	case <-capped:
	default:
		t.Fatal("cap callback did not fire")
	}

	require.NoError(t, collector.Close())

	data, err := os.ReadFile(filepath.Join(dir, "out.output"))
	require.NoError(t, err)
	// Writes past the cap are discarded; the first write kept its partial fit.
	assert.Equal(t, "0123456789ABCDEF"+overflowMarker, string(data))
}

func TestClassifyExitMapsWriterFailureToTypedCaptureOutcome(t *testing.T) {
	t.Parallel()

	state, exitCode := classifyExit(errors.New("write output: disk full"))
	assert.Equal(t, StateOutputDrainTimeout, state)
	assert.Nil(t, exitCode)
}

func TestExtractTail(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "big.txt")
	var content []byte

	for i := range 2000 {
		content = append(content, []byte("line "+string(rune('a'+i%26))+"\n")...)
	}

	require.NoError(t, os.WriteFile(path, content, 0o600))

	tail, _, ok := ExtractTail(path, 50, 8*1024)
	require.True(t, ok)

	lines := splitCount(tail)
	assert.LessOrEqual(t, lines, 50)
	assert.Contains(t, tail, "line")

	// Binary files are rejected.
	binPath := filepath.Join(dir, "bin.output")
	require.NoError(t, os.WriteFile(binPath, []byte{0, 1, 2, 3, 0, 0, 0, 0}, 0o600))

	_, _, ok = ExtractTail(binPath, 50, 8*1024)
	assert.False(t, ok)

	// A missing file is rejected.
	_, _, ok = ExtractTail(filepath.Join(dir, "missing"), 50, 8*1024)
	assert.False(t, ok)
}

func splitCount(text string) int {
	count := 0

	for _, r := range text {
		if r == '\n' {
			count++
		}
	}

	if len(text) > 0 && text[len(text)-1] != '\n' {
		count++
	}

	return count
}

func TestService_StopRacingStartCannotMissProcess(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	var fence sync.Mutex
	admitted := make(chan struct{})
	spawn := make(chan struct{})
	service := newTestService(t, store, nil, func(context.Context, int64) (func(), error) {
		fence.Lock()

		return fence.Unlock, nil
	})

	startDone := make(chan error, 1)
	go func() {
		_, err := service.Start(ctx, testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
			close(admitted)
			<-spawn

			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
		startDone <- err
	}()

	<-admitted
	cancelled := make(chan int, 1)
	stopErr := make(chan error, 1)
	go func() {
		fence.Lock()
		defer fence.Unlock()

		count, err := service.CancelTree(ctx, 1, IntentSessionStopped)
		cancelled <- count
		stopErr <- err
	}()

	select {
	case <-cancelled:
		t.Fatal("stop crossed process admission before the ledger insert")
	case <-time.After(50 * time.Millisecond):
	}

	close(spawn)
	require.NoError(t, <-startDone)
	assert.Equal(t, 1, <-cancelled)
	require.NoError(t, <-stopErr)

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 5*time.Second, 20*time.Millisecond)
}

func TestService_ShutdownRacingInsertCannotMissProcess(t *testing.T) {
	ctx := context.Background()
	store := &blockingInsertStore{
		Store: newTestStore(t), reached: make(chan Process, 1), proceed: make(chan struct{}),
		failRecordIntent: true,
	}
	service := newTestService(t, store, nil, nil)
	startDone := make(chan error, 1)

	go func() {
		_, err := service.Start(ctx, testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
		startDone <- err
	}()

	record := <-store.reached
	cancelDone := make(chan int, 1)
	cancelErr := make(chan error, 1)
	go func() {
		cancelled, err := service.CancelAll(ctx, IntentDaemonShutdown)
		cancelDone <- cancelled
		cancelErr <- err
	}()

	select {
	case <-cancelDone:
		t.Fatal("shutdown returned while an admitted process had not joined")
	case <-time.After(50 * time.Millisecond):
	}

	close(store.proceed)
	require.ErrorIs(t, <-startDone, ErrFenced)
	assert.Equal(t, 0, <-cancelDone, "the ledger row was not visible to the initial snapshot")
	require.NoError(t, <-cancelErr)
	final := waitState(t, store, record.ID, StateInterrupted, 5*time.Second)
	assert.Equal(t, IntentDaemonShutdown, final.HostIntent)

	assert.Eventually(t, func() bool {
		return service.liveCount(2) == 0
	}, 5*time.Second, 20*time.Millisecond)
}

func TestService_ShutdownRacingFastInsertInterruptsBeforeSupervision(t *testing.T) {
	ctx := context.Background()
	store := &blockingInsertStore{
		Store: newTestStore(t), reached: make(chan Process, 1), proceed: make(chan struct{}),
		failRecordIntent: true,
	}
	service := newTestService(t, store, nil, nil)
	startDone := make(chan error, 1)

	go func() {
		_, err := service.Start(ctx, testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "true"), nil
		})
		startDone <- err
	}()

	record := <-store.reached
	cancelDone := make(chan error, 1)
	go func() {
		_, err := service.CancelAll(ctx, IntentDaemonShutdown)
		cancelDone <- err
	}()
	require.Eventually(t, func() bool {
		return service.admissionClosed()
	}, time.Second, 10*time.Millisecond)

	close(store.proceed)
	require.ErrorIs(t, <-startDone, ErrFenced)
	// Shutdown may snapshot before or after the inserted row; a visible row
	// surfaces the injected persistence error while both paths still converge.
	if err := <-cancelDone; err != nil {
		require.ErrorContains(t, err, "injected shutdown intent failure")
	}
	final := waitState(t, store, record.ID, StateInterrupted, 5*time.Second)
	assert.Equal(t, IntentDaemonShutdown, final.HostIntent)
}

func TestService_RepeatedInterruptIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	spec := testSpec(2)

	record, err := service.Start(ctx, spec,
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	first, err := service.InterruptNonterminal(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, first)

	second, err := service.InterruptNonterminal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second, "an interrupted record must not re-interrupt")

	final, err := store.GetProcess(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, StateInterrupted, final.State)
	assert.NotNil(t, final.FinishedAt)
}

func TestService_RestartSweepDoesNotDuplicateJoinedShutdown(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	outputDir := t.TempDir()

	first := NewService(store, Options{OutputDir: outputDir, GuardianCommand: testGuardianCommand})
	spec := testSpec(2)

	record, err := first.Start(ctx, spec,
		func(ctx context.Context) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "sleep", "30"), nil
		})
	require.NoError(t, err)

	_, err = first.CancelAll(ctx, IntentDaemonShutdown)
	require.NoError(t, err)

	second := NewService(store, Options{OutputDir: outputDir, GuardianCommand: testGuardianCommand})

	count, err := second.InterruptNonterminal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	final, err := store.GetProcess(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, StateInterrupted, final.State)

	// Partial output is preserved.
	data, err := os.ReadFile(record.OutputPath)
	require.NoError(t, err, "a partial output file survives the restart sweep")

	_ = data
}

func isolatedGuardianEnv(environ []string, home string) []string {
	keys := []string{"HOME", "USERPROFILE", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}
	replaced := make(map[string]bool, len(keys))
	for _, key := range keys {
		replaced[key] = true
	}

	result := make([]string, 0, len(environ)+len(keys))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if !replaced[key] {
			result = append(result, entry)
		}
	}

	values := []string{
		home,
		home,
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".config"),
		filepath.Join(home, ".local", "share"),
		filepath.Join(home, ".local", "state"),
	}
	for i, key := range keys {
		result = append(result, key+"="+values[i])
	}

	return result
}

func testGuardianCommand(guardPath string, readyWriter, leaseReader *os.File) *exec.Cmd {
	cmd := exec.Command( //nolint:gosec // The current test binary is the hermetic helper.
		os.Args[0], "-test.run=^TestProcessGuardianHelper$", "--", guardianMode, guardPath,
	)
	cmd.Env = append(isolatedGuardianEnv(os.Environ(), filepath.Dir(guardPath)),
		"COAGENT_TEST_PROCESS_GUARDIAN=1")
	cmd.ExtraFiles = []*os.File{readyWriter, leaseReader}

	return cmd
}

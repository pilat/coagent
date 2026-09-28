package sessionlifecycle

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

type manualBudgetTimer struct {
	ticks  chan time.Time
	resets chan time.Duration
}

type budgetObservationStub struct {
	budget.Service
	list        func(context.Context) ([]*sessionstore.BudgetRecord, error)
	listPending func(context.Context) ([]*sessionstore.BudgetRecord, error)
	observe     func(context.Context, int64) (*sessionstore.BudgetRecord, bool, error)
}

func TestBudgetReconcilerRetriesPendingParkOnSuccessiveTicks(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"requested", "draining"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			store, db, armed := newReconcilerBudget(t)
			fired, didFire, err := store.ObserveBudget(t.Context(), armed.RootSessionID,
				armed.ArmedAt.Add(time.Minute), "")
			require.NoError(t, err)
			require.True(t, didFire)
			require.Equal(t, sessionstore.BudgetFired, fired.State)
			if phase == "draining" {
				fired, err = store.BeginBudgetDrain(t.Context(), fired.RootSessionID,
					fired.Generation, fired.ParkOwner)
				require.NoError(t, err)
			}
			require.Equal(t, phase, fired.ParkPhase)

			parked := make(chan *sessionstore.BudgetRecord, 3)
			r := NewBudgetReconciler(budget.New(store), func(record *sessionstore.BudgetRecord) {
				parked <- record
			}).(*budgetReconciler)
			r.now = func() time.Time { return armed.ArmedAt.Add(time.Minute) }
			timer := newManualBudgetTimer()
			r.timer = func(delay time.Duration) budgetTimer {
				timer.resets <- delay
				return timer
			}
			t.Cleanup(func() { stopBudgetReconciler(t, r) })
			r.Start(t.Context())
			require.Zero(t, receiveBudgetValue(t, timer.resets))

			for range 3 {
				timer.ticks <- r.now()
				retry := receiveBudgetValue(t, parked)
				assert.Equal(t, fired.RootSessionID, retry.RootSessionID)
				assert.Equal(t, fired.Generation, retry.Generation)
				assert.Equal(t, phase, retry.ParkPhase)
				assert.Equal(t, budgetRetryInterval, receiveBudgetValue(t, timer.resets))
			}

			persisted, err := store.GetBudget(t.Context(), armed.RootSessionID)
			require.NoError(t, err)
			assert.Equal(t, phase, persisted.ParkPhase)
			var checkpoints int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox
				WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`,
				armed.RootSessionID).Scan(&checkpoints))
			assert.Equal(t, 1, checkpoints)
		})
	}
}

func TestBudgetReconcilerRestoresDeadlineWithoutProgressOrModel(t *testing.T) {
	t.Parallel()
	store, db, armed := newReconcilerBudget(t)
	var now atomic.Int64
	now.Store(armed.ArmedAt.UnixNano())
	parked := make(chan *sessionstore.BudgetRecord, 1)
	r := NewBudgetReconciler(budget.New(store), func(record *sessionstore.BudgetRecord) { parked <- record }).(*budgetReconciler)
	r.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	timer := newManualBudgetTimer()
	r.timer = func(delay time.Duration) budgetTimer {
		timer.resets <- delay
		return timer
	}
	t.Cleanup(func() { stopBudgetReconciler(t, r) })

	require.NoError(t, r.ReconcileArmed(t.Context()))
	r.Start(t.Context())
	r.Start(t.Context())
	require.Equal(t, time.Duration(0), receiveBudgetValue(t, timer.resets))
	timer.ticks <- r.now()
	require.Equal(t, time.Minute, receiveBudgetValue(t, timer.resets))

	now.Store(armed.ArmedAt.Add(time.Minute).UnixNano())
	timer.ticks <- r.now()
	fired := receiveBudgetValue(t, parked)
	assert.Equal(t, armed.Generation, fired.Generation)
	assert.Equal(t, "duration", fired.FiredReason)
	assert.Equal(t, "requested", fired.ParkPhase)
	receiveBudgetValue(t, timer.resets)

	require.NoError(t, r.ReconcileArmed(t.Context()))
	var checkpoints int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, armed.RootSessionID).Scan(&checkpoints))
	assert.Equal(t, 1, checkpoints)
	select {
	case <-parked:
		t.Fatal("an already fired generation was parked twice")
	default:
	}
}

func TestBudgetReconcilerWakeReplacesDeadlineAndStopSealsStart(t *testing.T) {
	t.Parallel()
	store, db, armed := newReconcilerBudget(t)
	r := NewBudgetReconciler(budget.New(store), func(*sessionstore.BudgetRecord) {}).(*budgetReconciler)
	r.now = func() time.Time { return armed.ArmedAt }
	timer := newManualBudgetTimer()
	r.timer = func(delay time.Duration) budgetTimer {
		timer.resets <- delay
		return timer
	}
	t.Cleanup(func() { stopBudgetReconciler(t, r) })
	r.Start(t.Context())
	receiveBudgetValue(t, timer.resets)
	timer.ticks <- r.now()
	require.Equal(t, time.Minute, receiveBudgetValue(t, timer.resets))

	_, err := db.ExecContext(t.Context(), `UPDATE session_budgets SET duration_seconds = 120,
		generation = generation + 1 WHERE root_session_id = ?`, armed.RootSessionID)
	require.NoError(t, err)
	r.Wake()
	require.Equal(t, time.Duration(0), receiveBudgetValue(t, timer.resets))
	timer.ticks <- r.now()
	require.Equal(t, 2*time.Minute, receiveBudgetValue(t, timer.resets))
	require.NoError(t, r.Stop(t.Context()))
	r.Start(t.Context())
	require.ErrorIs(t, r.ReconcileArmed(t.Context()), ErrShuttingDown)
}

func TestBudgetReconcilerStopCancelsAndJoinsStartupScan(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	exited := make(chan struct{})
	service := budgetObservationStub{list: func(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	}}
	r := NewBudgetReconciler(service, func(*sessionstore.BudgetRecord) { t.Error("unexpected park") })
	result := make(chan error, 1)
	go func() { result <- r.ReconcileArmed(t.Context()) }()
	receiveBudgetValue(t, entered)
	require.NoError(t, r.Stop(t.Context()))
	select {
	case <-exited:
	default:
		t.Fatal("stop returned before the startup scan joined")
	}
	require.ErrorIs(t, receiveBudgetValue(t, result), context.Canceled)
	r.Start(t.Context())
	require.NoError(t, r.Stop(t.Context()))
}

func TestBudgetReconcilerStopJoinsDeadlineWorkerAfterStartupContextExpires(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	exited := make(chan struct{})
	service := budgetObservationStub{list: func(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	}}
	r := NewBudgetReconciler(service, func(*sessionstore.BudgetRecord) { t.Error("unexpected park") })
	t.Cleanup(func() { stopBudgetReconciler(t, r) })
	startup, cancel := context.WithCancel(t.Context())
	cancel()
	r.Start(startup)
	receiveBudgetValue(t, entered)
	stopBudgetReconciler(t, r)
	select {
	case <-exited:
	default:
		t.Fatal("stop returned before the deadline worker joined")
	}
}

func TestBudgetReconcilerContinuesAfterRootFailureAndSkipsDrainingGeneration(t *testing.T) {
	t.Parallel()
	broken := errors.New("unreadable budget")
	service := budgetObservationStub{
		list: func(context.Context) ([]*sessionstore.BudgetRecord, error) {
			return []*sessionstore.BudgetRecord{{RootSessionID: 1}, {RootSessionID: 2}, {RootSessionID: 3}}, nil
		},
		observe: func(_ context.Context, rootID int64) (*sessionstore.BudgetRecord, bool, error) {
			if rootID == 1 {
				return nil, false, broken
			}
			phase := "requested"
			if rootID == 2 {
				phase = "draining"
			}
			return &sessionstore.BudgetRecord{
				RootSessionID: rootID,
				State:         sessionstore.BudgetFired,
				ParkPhase:     phase,
			}, true, nil
		},
	}
	var parked []int64
	r := NewBudgetReconciler(
		service,
		func(record *sessionstore.BudgetRecord) { parked = append(parked, record.RootSessionID) },
	)
	require.ErrorIs(t, r.ReconcileArmed(t.Context()), broken)
	assert.Equal(t, []int64{3}, parked)
	require.NoError(t, r.Stop(t.Context()))
}

func TestBudgetReconcilerRetriesAfterPanic(t *testing.T) {
	t.Parallel()
	calls := 0
	service := budgetObservationStub{list: func(context.Context) ([]*sessionstore.BudgetRecord, error) {
		calls++
		if calls == 1 {
			panic("broken projection")
		}
		return nil, nil
	}}
	r := NewBudgetReconciler(service, func(*sessionstore.BudgetRecord) {}).(*budgetReconciler)
	assert.Equal(t, budgetRetryInterval, r.reconcileSafely(t.Context()))
	assert.Equal(t, budgetRescanInterval, r.reconcileSafely(t.Context()))
	require.NoError(t, r.Stop(t.Context()))
}

func (t *manualBudgetTimer) C() <-chan time.Time            { return t.ticks }
func (t *manualBudgetTimer) Stop() bool                     { return true }
func (t *manualBudgetTimer) Reset(delay time.Duration) bool { t.resets <- delay; return true }

func (s budgetObservationStub) ListArmed(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
	return s.list(ctx)
}

func (s budgetObservationStub) ListPendingParks(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
	if s.listPending == nil {
		return nil, nil
	}
	return s.listPending(ctx)
}

func (s budgetObservationStub) Observe(
	ctx context.Context,
	rootID int64,
	_ float64,
	_ time.Time,
	_ string,
) (*sessionstore.BudgetRecord, bool, error) {
	return s.observe(ctx, rootID)
}

func newManualBudgetTimer() *manualBudgetTimer {
	return &manualBudgetTimer{ticks: make(chan time.Time, 1), resets: make(chan time.Duration, 4)}
}

func stopBudgetReconciler(t *testing.T, r BudgetReconciler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Stop(ctx))
}

func receiveBudgetValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("budget worker did not reach the expected boundary")
		var zero T
		return zero
	}
}

func newReconcilerBudget(t *testing.T) (sessionstore.Store, *sql.DB, *sessionstore.BudgetRecord) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "budget.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, dbPath))
	_, err = db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	root, err := store.CreateSession(ctx, 1, "model", "", map[string]any{"manager_id": "test:main"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget activate", sessionstore.ActivationDraft{
		ToolID: "set_budget", Command: "/budget",
	})
	require.NoError(t, err)
	seconds := int64(60)
	armed, _, err := store.ArmBudget(ctx, sessionstore.BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget", ToolCallID: "arm",
		DurationSeconds: &seconds, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	return store, db, armed
}

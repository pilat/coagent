package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// TestReadinessSuppressesIdleWhileRootIsActiveLoop pins plan decision 39: a
// delivered releasing output must not publish idle for a root a queued user
// input already reactivated; the idle surfaces only once the loop is gone.
func TestReadinessSuppressesIdleWhileRootIsActiveLoop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "readiness.db"))
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, filepath.Join(t.TempDir(), "unused.db")))
	t.Cleanup(func() { _ = db.Close() })

	sessions := sessionstore.NewStore(db)
	store := sessionstore.NewStore(db)
	projectID := testProject(t, store, "/tmp/readiness-fixture")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-readiness",
	})
	require.NoError(t, err)
	sessionID := record.ID

	var outputID int64
	require.NoError(t, db.QueryRow(`INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input, state,
		 attempt_seq, last_attempt_at, delivered_at, last_error)
		VALUES (?, 'message_persistent', 'final', '{}', 'test:final', 'fp', datetime('now'), 1, 'delivered',
		 1, datetime('now'), datetime('now'), '')
		RETURNING id`,
		sessionID).Scan(&outputID))

	mgr, _ := newScenarioDaemon(
		context.Background(),
		scriptedBuildInput(
			t,
			&config.Config{Model: "fake-model"},
			sessions,
			nil,
			func(*config.Config) (llm.Client, error) { return &scriptedLLM{respond: trivialRespond}, nil },
		),
		sessions,
		subagent.NewStore(db, store),
		nil,
		nil,
		nil,
		db,
	)
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()

	active := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, active)
	require.True(t, registered)

	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))
	requireNoManagerNotification(t, notifications)

	mgr.removeRunner(ctx, active)
	require.False(t, mgr.HasActiveLoop(sessionID))

	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))

	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)

	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))
	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)
	requireNoManagerNotification(t, notifications)
}

// The runner-teardown reconcile must consult the latest releasing output and
// publish idle for it once the live loop is gone.
func TestReconcileLatestReadinessPublishesIdleAfterTeardown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "readiness2.db"))
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, filepath.Join(t.TempDir(), "unused.db")))
	t.Cleanup(func() { _ = db.Close() })

	sessions := sessionstore.NewStore(db)
	store := sessionstore.NewStore(db)
	projectID := testProject(t, store, "/tmp/readiness-fixture")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-readiness",
	})
	require.NoError(t, err)

	var outputID int64
	require.NoError(t, db.QueryRow(`INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input, state,
		 attempt_seq, last_attempt_at, delivered_at, last_error)
		VALUES (?, 'message_persistent', 'final', '{}', 'test:final', 'fp', datetime('now'), 1, 'delivered',
		 1, datetime('now'), datetime('now'), '')
		RETURNING id`,
		record.ID).Scan(&outputID))

	mgr, _ := newScenarioDaemon(
		context.Background(),
		scriptedBuildInput(
			t,
			&config.Config{Model: "fake-model"},
			sessions,
			nil,
			func(*config.Config) (llm.Client, error) { return &scriptedLLM{respond: trivialRespond}, nil },
		),
		sessions,
		subagent.NewStore(db, store),
		nil,
		nil,
		nil,
		db,
	)
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()

	active := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, active)
	require.True(t, registered)
	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)
	requireNoManagerNotification(t, notifications)

	mgr.removeRunner(ctx, active)
	require.False(t, mgr.HasActiveLoop(record.ID))

	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)

	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)
}

func TestOwnerlessIdleIsSuppressedByReplacementRunner(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	defer mgr.Shutdown(time.Second)

	ctx := context.Background()
	projectID := testProject(t, projects, "/tmp/replacement-idle")
	record, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	notifications := mgr.bus.SubscribeAll()

	replacement := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, replacement)
	require.True(t, registered)

	mgr.publishOwnerlessIdle(ctx, record.ID)
	requireNoNotification(t, notifications)

	mgr.removeRunner(ctx, replacement)
	replacement.Complete()
}

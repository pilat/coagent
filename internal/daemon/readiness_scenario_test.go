package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
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
	store := NewStore(db)
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

	mgr := newSvc(
		&mockFactory{},
		store,
		sessions,
		sessions,
		subagent.NewStore(db),
		subagent.NewTransactions(db),
		nil,
		sessions,
		nil,
		nil,
	)
	controllers := NewController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()

	mgr.mu.Lock()
	mgr.loops[sessionID] = &runner{done: make(chan struct{})}
	mgr.mu.Unlock()

	require.NoError(t, mgr.ReconcileOutputReadiness(ctx, outputID))
	requireNoManagerNotification(t, notifications)

	mgr.mu.Lock()
	delete(mgr.loops, sessionID)
	mgr.mu.Unlock()

	require.NoError(t, mgr.ReconcileOutputReadiness(ctx, outputID))

	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)
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
	store := NewStore(db)
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

	mgr := newSvc(
		&mockFactory{},
		store,
		sessions,
		sessions,
		subagent.NewStore(db),
		subagent.NewTransactions(db),
		nil,
		sessions,
		nil,
		nil,
	)
	controllers := NewController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()

	mgr.mu.Lock()
	mgr.loops[record.ID] = &runner{done: make(chan struct{})}
	mgr.mu.Unlock()
	mgr.reconcileLatestReadiness(ctx, record.ID)
	requireNoManagerNotification(t, notifications)

	mgr.mu.Lock()
	delete(mgr.loops, record.ID)
	mgr.mu.Unlock()

	mgr.reconcileLatestReadiness(ctx, record.ID)

	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)
}

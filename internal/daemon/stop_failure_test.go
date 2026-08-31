package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// failingGetSessionStore makes exactly one GetSession call fail, simulating a
// transient store hiccup while everything else delegates to the real store.
type failingGetSessionStore struct {
	sessionstore.OrchestrationStore
	err     error
	pending atomic.Bool
}

func (s *failingGetSessionStore) GetSession(
	ctx context.Context,
	id int64,
) (*sessionstore.SessionRecord, error) {
	if s.pending.CompareAndSwap(true, false) {
		return nil, s.err
	}

	return s.OrchestrationStore.GetSession(ctx, id)
}

// A transient store failure while loading the session must not classify an
// owned session as ownerless: the idle publication is skipped, not faked.
func TestStopOnStoreFailureDoesNotPublishIdle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "stopfail.db"))
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, filepath.Join(t.TempDir(), "unused.db")))
	t.Cleanup(func() { _ = db.Close() })

	sessions := sessionstore.NewStore(db)
	store := NewStore(db)
	projectID := testProject(t, store, "/tmp/stop-failure")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-stop",
	})
	require.NoError(t, err)

	failing := &failingGetSessionStore{
		OrchestrationStore: sessions,
		err:                errors.New("disk hiccup"),
	}
	failing.pending.Store(true)
	mgr := newSvc(
		&mockFactory{}, store, failing, sessions, subagent.NewStore(db), subagent.NewTransactions(db), nil, nil, nil,
	)
	controllers := NewController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-stop").Subscribe()

	require.NoError(t, mgr.Stop(ctx, record.ID, 0),
		"the stop itself must succeed: the cleanup ran on the real store")

	// The unconditional stop announcement is legitimate; the idle publication
	// must be the one thing a failed ownership lookup suppresses.
	stopping := requireManagerNotification(t, notifications)
	require.Equal(t, sessionevent.NotifyMessage, stopping.Notification.Type)
	requireNoManagerNotification(t, notifications)
}

package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestPublishGate_RootPasses(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	pid := testProject(t, store, "/tmp/publish-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)

	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})

	sn := requireNotification(t, ch)
	assert.Equal(t, rec.ID, sn.SessionID)
	assert.Equal(t, "hi", sn.Notification.Message)
}

func TestPublishGate_RoutesRootOnlyToOwningManager(t *testing.T) {
	mgr, _, store := newTestManager(t)
	alpha := mgr.bus.SubscribeManager("alpha")
	beta := mgr.bus.SubscribeManager("beta")

	pid := testProject(t, store, "/tmp/publish-owned-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)

	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "private"})

	assert.Equal(t, rec.ID, requireManagerNotification(t, alpha).SessionID)
	requireNoManagerNotification(t, beta)
}

func TestPublishGate_OwnerlessRootReachesNoManager(t *testing.T) {
	mgr, _, store := newTestManager(t)
	alpha := mgr.bus.SubscribeManager("alpha")

	pid := testProject(t, store, "/tmp/publish-ownerless-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)

	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "unowned"})

	requireNoManagerNotification(t, alpha)
}

func TestPublishGate_ClaimingOwnerUpdatesTheWarmRoute(t *testing.T) {
	mgr, _, store := newTestManager(t)
	alpha := mgr.bus.SubscribeManager("alpha")

	pid := testProject(t, store, "/tmp/publish-claimed-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "before"})
	requireNoManagerNotification(t, alpha)

	require.NoError(t, mgr.SetAttributes(context.Background(), rec.ID, map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	}))
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "after"})

	assert.Equal(t, "after", requireManagerNotification(t, alpha).Notification.Message)
}

func TestPublishGate_ConcurrentClaimWinsOverAStaleRouteRead(t *testing.T) {
	mgr, _, store := newTestManager(t)
	alpha := mgr.bus.SubscribeManager("alpha")
	pid := testProject(t, store, "/tmp/publish-concurrent-claim")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)

	stale := &staleReadSessionStore{
		Store:  mgr.store,
		target: rec.ID,
		read:   make(chan struct{}), release: make(chan struct{}),
	}
	mgr.store = stale
	published := make(chan struct{})

	go func() {
		defer close(published)
		mgr.NotifySession(rec.ID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: "claimed while lookup was stale",
		})
	}()

	requireSignal(t, stale.read)
	require.NoError(t, mgr.SetAttributes(context.Background(), rec.ID, map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	}))
	close(stale.release)
	requireSignal(t, published)

	assert.Equal(t, "claimed while lookup was stale", requireManagerNotification(t, alpha).Notification.Message)
}

func TestPublishGate_DropsMalformedEventBeforeSessionLookup(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	counting := &countingSessionStore{Store: mgr.store}
	mgr.store = counting

	mgr.NotifySession(999, sessionevent.Notification{Type: sessionevent.NotifyStateChanged})

	requireNoNotification(t, ch)
	assert.Zero(t, counting.calls(), "invalid events must be rejected before routing reads durable state")
}

func TestPublishGate_ChildDropped(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	childID := newTestChild(t, mgr, store, "/tmp/publish-child")

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})

	requireNoNotification(t, ch)
}

func TestPublishGate_CachesChildVerdict(t *testing.T) {
	mgr, _, store := newTestManager(t)
	childID := newTestChild(t, mgr, store, "/tmp/publish-cache")

	counting := &countingSessionStore{Store: mgr.store}
	mgr.store = counting

	for range 2 {
		mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	}

	assert.Equal(t, 1, counting.calls(), "the child verdict is looked up once and cached")
}

// A lookup failure publishes anyway, and must NOT cache that fail-open answer:
// caching "root" for an actual child would leak its events until restart.
func TestPublishGate_FailOpenDoesNotPoisonCache(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	childID := newTestChild(t, mgr, store, "/tmp/publish-failopen")

	counting := &countingSessionStore{Store: mgr.store, failNth: 1}
	mgr.store = counting

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "first"})

	sn := requireNotification(t, ch)
	assert.Equal(t, "first", sn.Notification.Message, "a failed lookup fails open")

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "second"})

	requireNoNotification(t, ch)
	assert.Equal(t, 2, counting.calls(), "the failed lookup left the cache empty, so it is retried")
}

package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestPublishGate_RootPasses(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
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
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
	alpha := mgr.bus.SubscribeManager("alpha")
	beta := mgr.bus.SubscribeManager("beta")
	pid := testProject(t, store, "/tmp/publish-owned-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", managerAttrs("alpha"))
	require.NoError(t, err)
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "private"})
	assert.Equal(t, rec.ID, requireManagerNotification(t, alpha).SessionID)
	requireNoManagerNotification(t, beta)
}

func TestPublishGate_OwnerlessRootReachesNoManager(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
	alpha := mgr.bus.SubscribeManager("alpha")
	pid := testProject(t, store, "/tmp/publish-ownerless-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "unowned"})
	requireNoManagerNotification(t, alpha)
}

func TestPublishGate_ClaimingOwnerUpdatesTheWarmRoute(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
	alpha := mgr.bus.SubscribeManager("alpha")
	pid := testProject(t, store, "/tmp/publish-claimed-root")
	rec, err := mgr.store.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "before"})
	requireNoManagerNotification(t, alpha)
	require.NoError(t, mgr.SetAttributes(context.Background(), rec.ID, managerAttrs("alpha")))
	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "after"})
	assert.Equal(t, "after", requireManagerNotification(t, alpha).Notification.Message)
}

func TestPublishGate_ConcurrentClaimWinsOverAStaleRouteRead(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
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
	require.NoError(t, mgr.SetAttributes(context.Background(), rec.ID, managerAttrs("alpha")))
	close(stale.release)
	requireSignal(t, published)
	assert.Equal(t, "claimed while lookup was stale", requireManagerNotification(t, alpha).Notification.Message)
}

func TestPublishGate_DropsMalformedEventBeforeSessionLookup(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	ch := mgr.bus.SubscribeAll()
	counting := &countingSessionStore{Store: mgr.store}
	mgr.store = counting
	mgr.NotifySession(999, sessionevent.Notification{Type: sessionevent.NotifyStateChanged})
	requireNoNotification(t, ch)
	assert.Zero(t, counting.calls(), "invalid events must be rejected before routing reads durable state")
}

func TestPublishGate_ChildDropped(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	ch := mgr.bus.SubscribeAll()
	childID := newTestChild(t, h, "/tmp/publish-child")
	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	requireNoNotification(t, ch)
}

func TestPublishGate_CachesChildVerdict(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	childID := newTestChild(t, h, "/tmp/publish-cache")
	counting := &countingSessionStore{Store: mgr.store}
	mgr.store = counting
	for range 2 {
		mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	}
	assert.Equal(t, 1, counting.calls(), "the child verdict is looked up once and cached")
}

// A failed child lookup may publish once but must never cache a root verdict that leaks later events.
func TestPublishGate_FailOpenDoesNotPoisonCache(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	ch := mgr.bus.SubscribeAll()
	childID := newTestChild(t, h, "/tmp/publish-failopen")
	counting := &countingSessionStore{Store: mgr.store, failNth: 1}
	mgr.store = counting
	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "first"})
	sn := requireNotification(t, ch)
	assert.Equal(t, "first", sn.Notification.Message, "a failed lookup fails open")
	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "second"})
	requireNoNotification(t, ch)
	assert.Equal(t, 2, counting.calls(), "the failed lookup left the cache empty, so it is retried")
}

// Count and inject publish-routing reads while preserving the real store.
type countingSessionStore struct {
	Store

	mu       sync.Mutex
	getCalls int
	failNth  int // fail exactly the Nth GetSession call; 0 = never
}

type staleReadSessionStore struct {
	Store
	target      int64
	read        chan struct{}
	release     chan struct{}
	mu          sync.Mutex
	intercepted bool
}

func (c *countingSessionStore) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	c.mu.Lock()
	c.getCalls++
	n, fail := c.getCalls, c.failNth
	c.mu.Unlock()
	if fail > 0 && n == fail {
		return nil, errSessionRead
	}
	return c.Store.GetSession(ctx, id)
}

func (c *countingSessionStore) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCalls
}

func (s *staleReadSessionStore) GetSession(
	ctx context.Context,
	id int64,
) (*sessionstore.SessionRecord, error) {
	record, err := s.Store.GetSession(ctx, id)
	if err != nil || id != s.target {
		return record, err
	}
	s.mu.Lock()
	intercept := !s.intercepted
	s.intercepted = true
	s.mu.Unlock()
	if intercept {
		close(s.read)
		<-s.release
	}
	return record, nil
}

// errSessionRead is the sentinel the fail-open publish test asserts against.
var errSessionRead = errors.New("session store unavailable")

// newTestChild creates a root session and returns the ID of a subagent child of it.
func newTestChild(t *testing.T, h *harness, workDir string) int64 {
	t.Helper()
	ctx := context.Background()
	pid := testProject(t, h.store, workDir)
	parent, err := h.store.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)
	childID := h.createUnlinkedChild(parent.ID)
	return childID
}

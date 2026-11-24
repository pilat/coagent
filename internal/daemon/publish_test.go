package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestPublishGate_RootPasses(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	ch := mgr.bus.SubscribeAll()
	rec := h.createRoot(nil)
	mgr.NotifySession(rec, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	sn := requireNotification(t, ch)
	assert.Equal(t, rec, sn.SessionID)
	assert.Equal(t, "hi", sn.Notification.Message)
}

func TestPublishGate_RoutesRootToEveryManager(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	alpha := mgr.bus.SubscribeRoots()
	beta := mgr.bus.SubscribeRoots()
	rec := h.createRoot(nil)
	mgr.NotifySession(rec, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "private"})
	assert.Equal(t, rec, requireNotification(t, alpha).SessionID)
	assert.Equal(t, rec, requireNotification(t, beta).SessionID)
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
	childID := h.createUnlinkedChild(h.createRoot(nil))
	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	requireNoNotification(t, ch)
}

func TestPublishGate_CachesChildVerdict(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	childID := h.createUnlinkedChild(h.createRoot(nil))
	counting := &countingSessionStore{Store: mgr.store}
	mgr.store = counting
	for range 2 {
		mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	}
	assert.Equal(t, 1, counting.calls(), "the child verdict is looked up once and cached")
}

// Count publish-routing reads while preserving the real store.
type countingSessionStore struct {
	Store

	mu       sync.Mutex
	getCalls int
}

func (c *countingSessionStore) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	c.mu.Lock()
	c.getCalls++
	c.mu.Unlock()
	return c.Store.GetSession(ctx, id)
}

func (c *countingSessionStore) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCalls
}

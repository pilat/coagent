package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// errSessionRead is the sentinel the fail-open publish test asserts against.
var errSessionRead = errors.New("session store unavailable")

// countingSessionStore decorates a real session store so the publish gate's
// lookups can be counted and made to fail on demand.
type countingSessionStore struct {
	sessionstore.OrchestrationStore

	mu       sync.Mutex
	getCalls int
	failNth  int // fail exactly the Nth GetSession call; 0 = never
}

// eventCollector accumulates everything a SubscribeAll channel yields, so a test
// can assert on the whole stream after the fact instead of racing it.
type eventCollector struct {
	mu      sync.Mutex
	events  []controllerapi.SessionNotification
	done    chan struct{}
	changed chan struct{}
}

func TestPublishGate_RootPasses(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	pid := testProject(t, store, "/tmp/publish-root")
	rec, err := mgr.sessionStore.CreateSession(context.Background(), pid, "fake-model", "", nil)
	require.NoError(t, err)

	mgr.NotifySession(rec.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})

	sn := requireNotification(t, ch)
	assert.Equal(t, rec.ID, sn.SessionID)
	assert.Equal(t, "hi", sn.Notification.Message)
}

func TestPublishGate_DropsMalformedEventBeforeSessionLookup(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	counting := &countingSessionStore{OrchestrationStore: mgr.sessionStore}
	mgr.sessionStore = counting

	mgr.NotifySession(999, sessionevent.Notification{Type: sessionevent.NotifyStateChanged})

	requireNoNotification(t, ch)
	assert.Zero(t, counting.calls(), "invalid events must be rejected before routing reads durable state")
}

func TestPublishGate_ChildDropped(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	childID := newTestChild(t, mgr, store, "/tmp/publish-child")

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})

	requireNoNotification(t, ch)
}

func TestPublishGate_CachesChildVerdict(t *testing.T) {
	mgr, _, store := newTestManager(t)
	childID := newTestChild(t, mgr, store, "/tmp/publish-cache")

	counting := &countingSessionStore{OrchestrationStore: mgr.sessionStore}
	mgr.sessionStore = counting

	for range 2 {
		mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hi"})
	}

	assert.Equal(t, 1, counting.calls(), "the child verdict is looked up once and cached")
}

// A lookup failure publishes anyway, and must NOT cache that fail-open answer:
// caching "root" for an actual child would leak its events until restart.
func TestPublishGate_FailOpenDoesNotPoisonCache(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	childID := newTestChild(t, mgr, store, "/tmp/publish-failopen")

	counting := &countingSessionStore{OrchestrationStore: mgr.sessionStore, failNth: 1}
	mgr.sessionStore = counting

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "first"})

	sn := requireNotification(t, ch)
	assert.Equal(t, "first", sn.Notification.Message, "a failed lookup fails open")

	mgr.NotifySession(childID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "second"})

	requireNoNotification(t, ch)
	assert.Equal(t, 2, counting.calls(), "the failed lookup left the cache empty, so it is retried")
}

func TestSpawnedChildProducesNoPubSubEvents(t *testing.T) {
	h := newSubagentHarness(t)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn a child", "fake-model", nil)
	require.NoError(t, err)

	// Wait for the child to reach a terminal state: its loop — and with it
	// announceSession — must actually have run, or the assertion below is vacuous.
	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	var parentEvents int

	for _, sn := range collector.snapshot() {
		assert.NotEqual(t, link.ChildID, sn.SessionID, "child session events must never reach subscribers")

		if sn.SessionID == parentID {
			parentEvents++
		}
	}

	assert.Positive(t, parentEvents, "the parent's own events still flow")
}

func countPublishedMessage(events []controllerapi.SessionNotification, sessionID int64, message string) int {
	count := 0

	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyMessage &&
			event.Notification.Message == message {
			count++
		}
	}

	return count
}

func (c *countingSessionStore) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	c.mu.Lock()
	c.getCalls++
	n, fail := c.getCalls, c.failNth
	c.mu.Unlock()

	if fail > 0 && n == fail {
		return nil, errSessionRead
	}

	return c.OrchestrationStore.GetSession(ctx, id)
}

func (c *countingSessionStore) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.getCalls
}

func collectEvents(ch <-chan controllerapi.SessionNotification) *eventCollector {
	c := &eventCollector{done: make(chan struct{}), changed: make(chan struct{}, 1)}

	go func() {
		for {
			select {
			case sn, ok := <-ch:
				if !ok {
					return
				}

				c.mu.Lock()
				c.events = append(c.events, sn)
				c.mu.Unlock()

				select {
				case c.changed <- struct{}{}:
				default:
				}
			case <-c.done:
				return
			}
		}
	}()

	return c
}

func (c *eventCollector) snapshot() []controllerapi.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]controllerapi.SessionNotification, len(c.events))
	copy(out, c.events)

	return out
}

func (c *eventCollector) waitFor(
	t *testing.T,
	label string,
	condition func([]controllerapi.SessionNotification) bool,
) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		if condition(c.snapshot()) {
			return
		}

		select {
		case <-c.changed:
		case <-timer.C:
			t.Fatalf("timed out waiting for controller trace: %s; events: %+v", label, c.snapshot())
		}
	}
}

func (c *eventCollector) stop() { close(c.done) }

// newTestChild creates a root session and returns the ID of a subagent child of it.
func newTestChild(t *testing.T, mgr *svc, store Store, workDir string) int64 {
	t.Helper()

	ctx := context.Background()
	pid := testProject(t, store, workDir)

	parent, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := mgr.sessionStore.CreateSubagentSession(ctx, pid, parent.ID, parent.ID, "general", "fake-model", "")
	require.NoError(t, err)

	return childID
}

func requireNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) controllerapi.SessionNotification {
	t.Helper()

	select {
	case sn := <-ch:
		return sn
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}

	return controllerapi.SessionNotification{}
}

func requireNoNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) {
	t.Helper()

	select {
	case sn := <-ch:
		t.Fatalf("unexpected notification for session %d: %+v", sn.SessionID, sn.Notification)
	case <-time.After(200 * time.Millisecond):
	}
}

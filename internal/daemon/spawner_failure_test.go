package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

type stoppingGateStore struct {
	sessionstore.OrchestrationStore
	sessionID   int64
	written     chan struct{}
	release     chan struct{}
	writtenOnce sync.Once
	releaseOnce sync.Once
}

func (s *stoppingGateStore) UpdateSessionStatus(
	ctx context.Context,
	id int64,
	status sessionstore.SessionStatus,
) error {
	if err := s.OrchestrationStore.UpdateSessionStatus(ctx, id, status); err != nil {
		return err
	}

	if id == s.sessionID && status == sessionstore.SessionStatusStopping {
		s.writtenOnce.Do(func() { close(s.written) })
		<-s.release
	}

	return nil
}

type observedLock struct {
	permit    chan struct{}
	attempted chan struct{}
	acquired  chan struct{}
}

func (l *observedLock) Lock() {
	l.attempted <- struct{}{}
	<-l.permit
	l.acquired <- struct{}{}
}

func (l *observedLock) Unlock() { l.permit <- struct{}{} }

func requireBarrierSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestStopRejectsSpawnQueuedBehindDurableBoundary(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	store := &stoppingGateStore{
		OrchestrationStore: h.mgr.sessionStore,
		sessionID:          root.ID,
		written:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	lock := &observedLock{
		permit:    make(chan struct{}, 1),
		attempted: make(chan struct{}, 2),
		acquired:  make(chan struct{}, 2),
	}
	lock.permit <- struct{}{}
	<-lock.permit
	h.mgr.sessionStore = store
	h.mgr.treeMu = lock
	t.Cleanup(func() {
		store.releaseOnce.Do(func() { close(store.release) })
		h.mgr.sessionStore = store.OrchestrationStore
	})

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.mgr.Stop(context.Background(), root.ID) }()

	requireBarrierSignal(t, lock.attempted, "stop did not attempt the admission boundary")
	lock.permit <- struct{}{}
	requireBarrierSignal(t, lock.acquired, "stop did not acquire the admission boundary")
	requireBarrierSignal(t, store.written, "stop did not durably mark the root stopping")
	record, err := store.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopping, record.Status)

	spawnDone := make(chan error, 1)
	go func() {
		_, spawnErr := h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
		spawnDone <- spawnErr
	}()

	requireBarrierSignal(t, lock.attempted, "spawn did not queue behind the stop boundary")
	store.releaseOnce.Do(func() { close(store.release) })
	require.NoError(t, <-stopDone)
	require.ErrorContains(t, <-spawnDone, "not accepting subagents")

	records, err := store.ListAllSessions(h.ctx)
	require.NoError(t, err)
	assert.Len(t, records, 1, "the queued spawn must not create a child or link")
}

func TestSpawnRejectsStoppedParent(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, h.mgr.Stop(context.Background(), root.ID))

	_, err = h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.ErrorContains(t, err, "not accepting subagents")
}

// TestChildDepth_ReadErrorCancelsSpawn: a ledger read failure must not read as
// "parent has no link" — that would reset the nesting depth to 1 and let a spawn
// through that the cap should have rejected.
func TestChildDepth_ReadErrorCancelsSpawn(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner LinkStore) LinkStore {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	flaky.failGetLink(1, 0)

	_, err = h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.ErrorIs(t, err, errLinkRead)
}

// TestChildDepth_NoLinkKeepsDepthOne: the "no row" branch is the normal path for
// every root session and must stay untouched.
func TestChildDepth_NoLinkKeepsDepthOne(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	depth, err := h.mgr.childDepth(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, depth)

	child, err := h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)

	link, err := h.links.GetLink(h.ctx, child.ChildID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, 1, link.Depth)
}

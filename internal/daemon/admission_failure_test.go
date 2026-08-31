package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type childStateLinkStore struct {
	subagent.Store
	link *subagent.Link
}

func (s childStateLinkStore) GetLink(context.Context, int64) (*subagent.Link, error) {
	return s.link, nil
}

type childStateSessionStore struct {
	sessionstore.OrchestrationStore
	record *sessionstore.SessionRecord
	reads  int
}

func (s *childStateSessionStore) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	s.reads++

	return s.record, nil
}

// TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait preserves the
// crash obligation through an admission delay without relying on queue metadata.
func TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	ctx := context.Background()

	reserved := admission.MaxTotal
	for range reserved {
		require.True(t, mgr.admit.TryAdmit(admission.Parent, 0))
	}
	t.Cleanup(func() {
		for range reserved {
			mgr.admit.Release(admission.Parent, 0)
		}
		mgr.Shutdown(3 * time.Second)
	})

	projectID := testProject(t, projects, "/tmp/recovery-capacity")
	rec, err := mgr.sessionStore.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	input, err := mgr.inboxStore.EnqueueInput(ctx, rec.ID, sessionstore.InputSourceUser, "promoted before crash")
	require.NoError(t, err)
	_, err = mgr.inboxStore.PromoteInput(ctx, input.ID, "promoted before crash")
	require.NoError(t, err)

	sess := &mockSession{completeAfter: 10 * time.Millisecond}
	factory.nextSess = sess
	events := mgr.PubSub().SubscribeAll()

	require.NoError(t, mgr.ensureSessionRunner(ctx, rec.ID))
	assert.False(t, mgr.HasActiveLoop(rec.ID))
	require.Equal(t, 1, mgr.pendingQueue.Len())

	mgr.admit.Release(admission.Parent, 0)
	reserved--
	mgr.drainPendingRunners(ctx)
	waitForState(t, events, rec.ID, controllerapi.StateIdle, 3*time.Second)

	sess.mu.Lock()
	ran := sess.ran
	sess.mu.Unlock()
	assert.True(t, ran, "first runner must derive and execute the promoted user turn")
}

// TestDrainQueue_UnknownChildStateDefers: reading the failure as "terminated"
// would recurse and silently flush every live entry behind it.
func TestDrainQueue_UnknownChildStateDefers(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	for _, callID := range []string{"bg-1", "bg-2", "bg-3"} {
		childID, cerr := h.sessStore.CreateSubagentSession(
			h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
		)
		require.NoError(t, cerr)
		require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
			ParentID: parent.ID, ChildID: childID, TaskCallID: callID,
		}))
		h.mgr.enqueueChild(h.ctx, childID, parent.ID, "/tmp", h.projectID)
	}

	require.Equal(t, 3, h.queueLen())

	flaky.failGetLink(1, 0)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.drainQueue(ctx)

	assert.Equal(t, 3, h.queueLen(), "nothing is dropped and nothing recursed")
	assert.Zero(t, h.mgr.runners.Len(), "no runner was created")
	assert.NotEmpty(t, logs.FilterMessage("queued_child_state_unknown").All())
}

// TestChildTerminated_ReadErrorSurfaces: the mirrored form of the same defect —
// `err == nil && link != nil && Terminal()` reads a failed read as "not killed".
func TestChildTerminated_ReadErrorSurfaces(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	terminated, err := h.mgr.childTerminated(h.ctx, h.childID)
	require.NoError(t, err)
	assert.False(t, terminated, "a live queued child is not terminated")

	h.flaky.failGetLink(1, h.childID)

	_, err = h.mgr.childTerminated(h.ctx, h.childID)
	require.ErrorIs(t, err, errLinkRead)
}

func TestChildTerminated_ClassifiesLedgerBeforeSessionFallback(t *testing.T) {
	t.Parallel()

	killedAt := time.Now()
	tests := []struct {
		name         string
		link         *subagent.Link
		record       *sessionstore.SessionRecord
		want         bool
		wantSessRead int
	}{
		{
			name: "completed link", link: &subagent.Link{State: subagent.StateCompleted},
			want: true,
		},
		{
			name: "stopped link", link: &subagent.Link{State: subagent.StateStopped},
			want: true,
		},
		{
			name: "live link with killed session", link: &subagent.Link{State: subagent.StateRunning},
			record: &sessionstore.SessionRecord{KilledAt: &killedAt}, want: true, wantSessRead: 1,
		},
		{
			name: "missing link with live session", record: &sessionstore.SessionRecord{},
			wantSessRead: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := &childStateSessionStore{record: tt.record}
			mgr := &svc{links: childStateLinkStore{link: tt.link}, sessionStore: sessions}

			got, err := mgr.childTerminated(t.Context(), 42)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSessRead, sessions.reads)
		})
	}
}

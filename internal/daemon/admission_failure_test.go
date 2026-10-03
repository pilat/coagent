package daemon

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type childStateSessionStore struct {
	Store
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

	reserved := maxTotal
	for range reserved {
		require.True(t, mgr.runners.tryAdmit(false, 0))
	}
	t.Cleanup(func() {
		for range reserved {
			mgr.runners.release(false, 0)
		}
		mgr.Shutdown(3 * time.Second)
	})

	projectID := testProject(t, projects, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := mgr.store.Enqueue(
		ctx,
		sessionstore.Input{SessionID: rec.ID, Source: sessionstore.InputSourceUser, Content: "promoted before crash"},
	)
	require.NoError(t, err)
	_, err = mgr.store.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "promoted before crash",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)

	sess := &mockSession{completeAfter: 10 * time.Millisecond}
	factory.nextSess = sess
	events := mgr.bus.SubscribeAll()

	require.NoError(t, mgr.start(ctx, rec.ID))
	assert.False(t, mgr.HasActiveLoop(rec.ID))
	require.Equal(t, 1, runnerWaitingCount(mgr.runners))

	mgr.runners.release(false, 0)
	reserved--
	mgr.drain(ctx)
	waitForState(t, events, rec.ID, controllerapi.StateIdle, 3*time.Second)

	sess.mu.Lock()
	ran := sess.ran
	sess.mu.Unlock()
	assert.True(t, ran, "first runner must derive and execute the promoted user turn")
}

// A failed classification must preserve the entire waiting FIFO for retry.
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
		childID, cerr := func() (int64, error) {
			var id int64
			err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
				var err error
				id, err = sessionstore.CreateSubagentSessionTx(
					h.ctx,
					tx,
					sessionstore.CreateSubagentSession{
						ProjectID:      h.projectID,
						ParentID:       parent.ID,
						RootID:         parent.ID,
						AgentType:      "general",
						Model:          "fake-model",
						ReasoningLevel: "",
					},
				)
				return err
			})
			return id, err
		}()
		require.NoError(t, cerr)
		require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
			ParentID: parent.ID, ChildID: childID, TaskCallID: callID,
		}))
		h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent.ID, child: true})
	}

	require.Equal(t, 3, h.queueLen())

	flaky.failGetLink(1, 0)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.drain(ctx)

	assert.Equal(t, 3, h.queueLen(), "nothing is dropped and nothing recursed")
	assert.Zero(t, runnerLiveCount(h.mgr.runners), "no runner was created")
	assert.NotEmpty(t, logs.FilterMessage("waiting_runner_start_failed").All())

	flaky.mu.Lock()
	flaky.getLinkFailFrom = 0
	flaky.mu.Unlock()
	require.Eventually(t, func() bool {
		return h.queueLen() < 3 || runnerLiveCount(h.mgr.runners) > 0
	}, time.Second, 10*time.Millisecond, "the delayed retry must not wait for another slot release")
}

func TestStartSkipsTerminalAndStoppedChildren(t *testing.T) {
	for _, state := range []subagent.State{subagent.StateCompleted, subagent.StateStopped, subagent.StateKilled} {
		t.Run(string(state), func(t *testing.T) {
			mgr, _, projects := newTestManager(t)
			defer mgr.Shutdown(time.Second)

			ctx := context.Background()
			projectID := testProject(t, projects, t.TempDir())
			parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, seedChildLink(ctx, projects, subagent.Link{
				ParentID: parent.ID, ChildID: child.ID, TaskCallID: "terminal", State: state,
			}))

			require.NoError(t, mgr.start(ctx, child.ID))
			assert.False(t, mgr.HasActiveLoop(child.ID))
			assert.Zero(t, runnerRunningCount(mgr.runners))
		})
	}
}

func TestStartRejectsKilledSessionWithRunningChildLink(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	defer mgr.Shutdown(time.Second)

	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, projects, subagent.Link{
		ParentID: parent.ID, ChildID: child.ID, TaskCallID: "running", State: subagent.StateRunning,
	}))
	require.NoError(t, projects.WithTx(ctx, func(tx *sql.Tx) error {
		return sessionstore.MarkSessionKilledTx(ctx, tx, child.ID)
	}))

	err = mgr.start(ctx, child.ID)
	require.ErrorContains(t, err, "killed")
	assert.False(t, mgr.HasActiveLoop(child.ID))
	assert.Zero(t, runnerRunningCount(mgr.runners))
}

package daemon

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// ledgerHarness is a live daemon whose link store fails on demand, plus the ids
// of a parent and one non-terminal child of it.
type ledgerHarness struct {
	*subagentHarness

	flaky      *flakyLinkStore
	activation *flakyActivationStore
	parentID   int64
	childID    int64
}

type rejectingCompletionTransactions struct {
	subagent.Store
}

func (rejectingCompletionTransactions) DeliverCompletion(
	context.Context,
	subagent.Link,
	string,
) (bool, error) {
	return false, sessionstore.ErrSessionNotAcceptingInput
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()

	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	activation := &flakyActivationStore{Store: h.mgr.links}
	h.mgr.links = activation

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
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
	require.NoError(t, err)
	require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	return &ledgerHarness{
		subagentHarness: h, flaky: flaky, activation: activation,
		parentID: parent.ID, childID: childID,
	}
}

// drainNotifications collects everything buffered on a per-session subscription.
func drainNotifications(ch <-chan sessionevent.Notification) []sessionevent.Notification {
	var out []sessionevent.Notification

	for {
		select {
		case n := <-ch:
			out = append(out, n)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// TestFinalizeChild_LinkReadErrorStopsShort: an unreadable link is not "this is
// not a subagent" — nothing is written, and with no parent id the log is all.
func TestFinalizeChild_LinkReadErrorStopsShort(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	sub := h.mgr.bus.Subscribe(h.parentID)
	defer h.mgr.bus.Unsubscribe(h.parentID, sub)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.activation.failRead = true
	h.startInboxWake()
	finalizeTestChild(ctx, t, h.mgr, h.childID)

	assert.Equal(t, 1, h.activation.attempts(), "the failed read performs no transition")

	entries := logs.FilterMessage("finalize_child").All()
	require.Len(t, entries, 1)
	assert.Equal(t, h.childID, entries[0].ContextMap()["child"])

	assert.Empty(t, drainNotifications(sub), "no parent id is known, so nothing is published")
}

// TestFinalizeChild_NoLinkIsSilent: the "no row" branch is every root session's
// normal exit — it must stay completely quiet.
func TestFinalizeChild_NoLinkIsSilent(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	rec, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	sub := h.mgr.bus.Subscribe(rec.ID)
	defer h.mgr.bus.Unsubscribe(rec.ID, sub)

	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, rec.ID)

	assert.Zero(t, logs.Len(), "a root session's exit logs nothing")
	assert.Empty(t, drainNotifications(sub))
}

func TestFinalizeChild_WriteFailureRemainsRecoverable(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()
	h.activation.failN = -1
	sub := h.mgr.bus.Subscribe(h.parentID)
	defer h.mgr.bus.Unsubscribe(h.parentID, sub)
	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))
	h.startInboxWake()
	finalizeTestChild(ctx, t, h.mgr, h.childID)
	assert.Equal(t, 1, h.activation.attempts())
	assert.Len(t, logs.FilterMessage("finalize_child").All(), 1)
	notifications := drainNotifications(sub)
	require.NotEmpty(t, notifications)
	assert.Contains(t, notifications[0].Message, "completion could not be recorded")
	running, err := h.links.ListRunningChildLinks(h.ctx)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(running, func(link subagent.Link) bool { return link.ChildID == h.childID }))
}

func TestDeliverCompletionLogsRejectedParent(t *testing.T) {
	t.Parallel()

	killedAt := time.Now()
	sessions := &childStateSessionStore{record: &sessionstore.SessionRecord{KilledAt: &killedAt}}
	manager := &svc{store: sessions, links: rejectingCompletionTransactions{}}
	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	manager.deliverCompletionToParent(ctx, subagent.Link{
		ParentID: 7, ChildID: 8, ActivationSeq: 1, Blocking: true,
	})

	entries := logs.FilterMessage("deliver_completion_dropped").All()
	require.Len(t, entries, 1)
	assert.Equal(t, int64(8), entries[0].ContextMap()["child"])
	assert.Equal(t, int64(7), entries[0].ContextMap()["parent"])
}

func TestCompletionContentIncludesPersistedIteration(t *testing.T) {
	t.Parallel()

	manager := &svc{store: &childStateSessionStore{
		record: &sessionstore.SessionRecord{ID: 8, Iteration: 4},
	}}

	content := manager.completionContent(t.Context(), subagent.Link{
		ChildID: 8, State: subagent.StateCompleted, Outcome: subagent.OutcomeCompleted,
	})

	assert.Contains(t, content, "(4 iterations)")
}

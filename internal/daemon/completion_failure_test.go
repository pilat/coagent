package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
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

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()

	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	activation := &flakyActivationStore{Transactions: h.mgr.subagents}
	h.mgr.subagents = activation

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
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

	sub := h.mgr.PubSub().Subscribe(h.parentID)
	defer h.mgr.PubSub().Unsubscribe(h.parentID, sub)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.flaky.failGetLink(1, h.childID)
	h.mgr.finalizeChild(ctx, h.childID, false, false)

	assert.Zero(t, h.activation.attempts(), "nothing is written on an unreadable link")

	entries := logs.FilterMessage("finalize_get_link").All()
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

	sub := h.mgr.PubSub().Subscribe(rec.ID)
	defer h.mgr.PubSub().Unsubscribe(rec.ID, sub)

	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.finalizeChild(ctx, rec.ID, false, false)

	assert.Zero(t, logs.Len(), "a root session's exit logs nothing")
	assert.Empty(t, drainNotifications(sub))
}

// TestFinalizeChild_TerminalMarkRetries: a transient write failure must not
// leave the parent waiting until the daemon restarts.
func TestFinalizeChild_TerminalMarkRetries(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.activation.failN = 2

	start := time.Now()
	h.mgr.finalizeChild(h.ctx, h.childID, false, false)
	elapsed := time.Since(start)

	assert.Equal(t, 3, h.activation.attempts(), "two failures then a success")
	// 300ms is deliberate backoff (2×linkTerminalBackoff); the rest is headroom
	// for parallel-suite load. Bounded means "not until daemon restart".
	assert.Less(t, elapsed, 1500*time.Millisecond, "the retry budget stays bounded")

	link, err := h.links.GetLink(h.ctx, h.childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.True(t, link.Terminal(), "the link reached a terminal state")

	// deliverCompletionToParent ran: the idle parent was revived and consumed it.
	h.waitForDelivery(h.childID)
}

// TestFinalizeChild_TerminalMarkExhausted: the link stays non-terminal on purpose
// so the startup sweep can pick the child back up, and the parent is told.
func TestFinalizeChild_TerminalMarkExhausted(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.activation.failN = -1

	sub := h.mgr.PubSub().Subscribe(h.parentID)
	defer h.mgr.PubSub().Unsubscribe(h.parentID, sub)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.finalizeChild(ctx, h.childID, false, false)

	assert.Equal(t, linkTerminalAttempts, h.activation.attempts())
	assert.NotEmpty(t, logs.FilterMessage("mark_link_terminal").All())

	notifs := drainNotifications(sub)
	require.NotEmpty(t, notifs, "the parent is told")
	assert.Equal(t, sessionevent.NotifyMessage, notifs[0].Type)
	assert.Contains(t, notifs[0].Message, "Subagent")

	running, err := h.links.ListRunningChildLinks(h.ctx)
	require.NoError(t, err)
	assert.True(t,
		slices.ContainsFunc(running, func(l subagent.Link) bool { return l.ChildID == h.childID }),
		"a non-terminal link is still recoverable by the sweep",
	)
}

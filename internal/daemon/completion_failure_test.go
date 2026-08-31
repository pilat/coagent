package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

type pendingCompletionLinkStore struct {
	subagent.Store
	link  subagent.Link
	links []subagent.Link
}

func (s pendingCompletionLinkStore) GetLink(_ context.Context, childID int64) (*subagent.Link, error) {
	for _, candidate := range s.links {
		if candidate.ChildID == childID {
			link := candidate

			return &link, nil
		}
	}

	link := s.link

	return &link, nil
}

func (s pendingCompletionLinkStore) ListPendingChildLinks(context.Context, int64) ([]subagent.Link, error) {
	if len(s.links) > 0 {
		return append([]subagent.Link(nil), s.links...), nil
	}

	return []subagent.Link{s.link}, nil
}

type completionPersistProbe struct {
	sessionlifecycle.Completions
	err   error
	calls int
}

func (p *completionPersistProbe) Persist(
	context.Context,
	session.Service,
	subagent.Link,
	[]*transcript.Message,
) error {
	p.calls++

	return p.err
}

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
	h.mgr.completions = h.mgr.newCompletionCoordinator()

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

func TestDeliverCompletionLogsRejectedParent(t *testing.T) {
	t.Parallel()

	killedAt := time.Now()
	sessions := &childStateSessionStore{record: &sessionstore.SessionRecord{KilledAt: &killedAt}}
	manager := &svc{sessionStore: sessions}
	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	manager.deliverCompletionToParent(ctx, subagent.Link{
		ParentID: 7, ChildID: 8, ActivationSeq: 1,
	})

	entries := logs.FilterMessage("deliver_completion_dropped").All()
	require.Len(t, entries, 1)
	assert.Equal(t, int64(8), entries[0].ContextMap()["child"])
	assert.Equal(t, int64(7), entries[0].ContextMap()["parent"])
}

func TestInjectBlockingCompletionRejectsEitherContractMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		link   subagent.Link
		callID string
	}{
		{
			name:   "nonblocking link",
			link:   subagent.Link{ChildID: 8, TaskCallID: "call", ActivationSeq: 1},
			callID: "call",
		},
		{
			name:   "wrong call",
			link:   subagent.Link{ChildID: 8, TaskCallID: "other", Blocking: true, ActivationSeq: 1},
			callID: "call",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &svc{links: childStateLinkStore{link: &tt.link}}

			err := manager.injectBlockingCompletion(t.Context(), &mockSession{}, 8, tt.callID, 1)
			require.ErrorContains(t, err, "blocking completion contract mismatch")
		})
	}
}

func TestInjectOwedCompletionsPropagatesBlockingPersistenceFailure(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("persist completion failed")
	link := subagent.Link{
		ParentID: 7, ChildID: 8, TaskCallID: "call", Blocking: true,
		State: subagent.StateCompleted, ActivationSeq: 1,
	}
	persist := &completionPersistProbe{err: persistErr}
	manager := &svc{
		links:       pendingCompletionLinkStore{link: link},
		completions: persist,
		sessionStore: &childStateSessionStore{
			record: &sessionstore.SessionRecord{ID: link.ChildID},
		},
	}
	sess := &mockSession{pendingCalls: []session.PendingToolCall{{ID: "call", Name: tool.IDTask}}}

	err := manager.injectOwedCompletions(t.Context(), sess, 7)
	require.ErrorIs(t, err, persistErr)
	assert.Equal(t, 1, persist.calls)
}

func TestInjectOwedCompletionsSkipsRunningChild(t *testing.T) {
	t.Parallel()

	for _, blocking := range []bool{true, false} {
		name := "background"
		if blocking {
			name = "blocking"
		}

		t.Run(name, func(t *testing.T) {
			link := subagent.Link{
				ParentID: 7, ChildID: 8, TaskCallID: "call", Blocking: blocking,
				State: subagent.StateRunning, ActivationSeq: 1,
			}
			persist := &completionPersistProbe{err: errors.New("unexpected persistence")}
			manager := &svc{
				links:       pendingCompletionLinkStore{link: link},
				completions: persist,
				sessionStore: &childStateSessionStore{
					record: &sessionstore.SessionRecord{ID: link.ChildID},
				},
			}
			sess := &mockSession{}
			if blocking {
				sess.pendingCalls = []session.PendingToolCall{{ID: "call", Name: tool.IDTask}}
			}

			require.NoError(t, manager.injectOwedCompletions(t.Context(), sess, 7))
			assert.Zero(t, persist.calls)
		})
	}
}

func TestInjectOwedCompletionsContinuesPastRunningChild(t *testing.T) {
	t.Parallel()

	for _, blocking := range []bool{true, false} {
		name := "background"
		if blocking {
			name = "blocking"
		}

		t.Run(name, func(t *testing.T) {
			links := []subagent.Link{
				{
					ParentID: 7, ChildID: 8, TaskCallID: "running", Blocking: blocking,
					State: subagent.StateRunning, ActivationSeq: 1,
				},
				{
					ParentID: 7, ChildID: 9, TaskCallID: "terminal", Blocking: blocking,
					State: subagent.StateCompleted, ActivationSeq: 1,
				},
			}
			persistErr := errors.New("terminal persistence reached")
			persist := &completionPersistProbe{err: persistErr}
			manager := &svc{
				links:       pendingCompletionLinkStore{links: links},
				completions: persist,
				sessionStore: &childStateSessionStore{
					record: &sessionstore.SessionRecord{ID: links[1].ChildID},
				},
			}
			sess := &mockSession{}
			if blocking {
				sess.pendingCalls = []session.PendingToolCall{{ID: "terminal", Name: tool.IDTask}}
			}

			err := manager.injectOwedCompletions(t.Context(), sess, 7)
			require.ErrorIs(t, err, persistErr)
			assert.Equal(t, 1, persist.calls)
		})
	}
}

func TestCompletionContentIncludesPersistedIteration(t *testing.T) {
	t.Parallel()

	manager := &svc{sessionStore: &childStateSessionStore{
		record: &sessionstore.SessionRecord{ID: 8, Iteration: 4},
	}}

	content := manager.completionContent(t.Context(), subagent.Link{
		ChildID: 8, State: subagent.StateCompleted, Outcome: subagent.OutcomeCompleted,
	})

	assert.Contains(t, content, "(4 iterations)")
}

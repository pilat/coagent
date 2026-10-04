package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

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

func TestPendingExternalCallsRetainsAtomicChildHandoff(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "handoff.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))
	sessions := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessions)
	projectID := testProject(t, sessions, t.TempDir())
	parent, err := sessions.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	encoded, err := json.Marshal([]llmwire.ToolCall{{ID: taskCallID, Name: tool.IDTask}})
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{
		SessionID: parent.ID,
		Messages:  []*transcript.Message{{Role: llmwire.RoleAssistant, ToolCalls: encoded}},
	})
	require.NoError(t, err)
	childID, err := links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "fake-model", TaskCallID: taskCallID,
		Blocking: true, Depth: 1, State: subagent.StateSpawned,
	})
	require.NoError(t, err)
	finalized, err := links.Finalize(ctx, childID, false)
	require.NoError(t, err)
	require.NotNil(t, finalized)
	link, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	handoff := &deliveringChildLinks{Store: links, link: *link}
	manager, _ := newScenarioDaemon(ctx, sessionbuild.BuildInput{
		Config: &config.Config{WorkDir: t.TempDir()}, Store: sessions,
	}, sessions, handoff, nil, nil, nil, db)

	owners, err := manager.callOwners(ctx, parent.ID)
	require.NoError(t, err)
	require.True(t, handoff.delivered)
	require.Equal(t, map[string]string{taskCallID: tool.IDTask}, owners)
	pendingLinks, err := links.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	require.Empty(t, pendingLinks)
	pendingInputs, err := sessions.ListPending(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, pendingInputs, 1)
	require.Equal(t, sessionstore.InputSourceCallResult, pendingInputs[0].Source)
	require.Equal(t, taskCallID, pendingInputs[0].Attributes["call_id"])
	require.Equal(t, tool.IDTask, pendingInputs[0].Attributes["tool_id"])
}

func TestLinkStore_CreateAndRead(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ls.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID, Model: "m",
		AgentType: "general", TaskCallID: "call-abc", Blocking: true, Depth: 1,
		State: subagent.StateSpawned,
	})
	require.NoError(t, err)

	got, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, parent.ID, got.ParentID)
	assert.Equal(t, childID, got.ChildID)
	assert.Equal(t, "call-abc", got.TaskCallID)
	assert.True(t, got.Blocking)
	assert.Equal(t, 1, got.Depth)
	assert.Equal(t, subagent.StateSpawned, got.State)
	assert.Zero(t, got.DeliveredAt)
	assert.Positive(t, got.CreatedAt)

	byCall, err := ls.GetLinkByTaskCallID(ctx, parent.ID, "call-abc")
	require.NoError(t, err)
	require.NotNil(t, byCall)
	assert.Equal(t, childID, byCall.ChildID)

	missing, err := ls.GetLink(ctx, 999999)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestLinkStore_DeliverBackgroundCompletionToInbox(t *testing.T) {
	ss, links, projectID := newTestLinkStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	link := subagent.Link{ParentID: parent.ID, ChildID: childID, TaskCallID: "call", Depth: 1}
	require.NoError(t, seedChildLink(ctx, ss, link))
	require.NoError(t, seedTerminalChild(ctx, ss, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted))
	storedLink, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, storedLink)
	won, err := links.DeliverBackgroundCompletion(ctx, *storedLink, 3)
	require.NoError(t, err)
	require.True(t, won)
	updated, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.Positive(t, updated.DeliveredInputID)
	assert.Zero(t, updated.DeliveredMsgID)

	input, err := ss.PeekPending(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceSubagent, input.Source)
	assert.EqualValues(t, childID, input.Attributes["child_id"])
	assert.EqualValues(t, 1, input.Attributes["activation_seq"])
	assert.Equal(t, `<subagent_completion>
child_id: `+strconv.FormatInt(childID, 10)+`
activation_seq: 1
outcome: completed
iterations: 3
result:
done
</subagent_completion>`, input.RawContent)

	won, err = links.DeliverBackgroundCompletion(ctx, *storedLink, 3)
	require.NoError(t, err)
	assert.False(t, won)
	second, err := ss.PeekPending(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, input.ID, second.ID)
}

func TestLinkStore_KilledParentRecoversBackgroundCompletionForSuppression(t *testing.T) {
	ss, links, projectID := newTestLinkStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	createTerminal := func(callID string, blocking bool) int64 {
		t.Helper()

		childID, createErr := func() (int64, error) {
			var id int64
			err := ss.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				id, err = sessionstore.CreateSubagentSessionTx(
					ctx,
					tx,
					sessionstore.CreateSubagentSession{
						ProjectID:      projectID,
						ParentID:       parent.ID,
						RootID:         parent.ID,
						AgentType:      "general",
						Model:          "m",
						ReasoningLevel: "",
					},
				)
				return err
			})
			return id, err
		}()
		require.NoError(t, createErr)
		require.NoError(t, seedChildLink(ctx, ss, subagent.Link{
			ParentID: parent.ID, ChildID: childID, TaskCallID: callID, Blocking: blocking,
		}))
		require.NoError(
			t,
			seedTerminalChild(ctx, ss, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted),
		)

		return childID
	}

	backgroundID := createTerminal("background", false)
	_ = createTerminal("blocking", true)
	require.NoError(
		t,
		ss.WithTx(ctx, func(tx *sql.Tx) error { return sessionstore.MarkSessionKilledTx(ctx, tx, parent.ID) }),
	)

	undelivered, err := links.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	assert.Equal(t, backgroundID, undelivered[0].ChildID)

	won, err := links.DeliverBackgroundCompletion(ctx, undelivered[0], 1)
	require.NoError(t, err)
	require.False(t, won)
	updated, err := links.GetLink(ctx, backgroundID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Positive(t, updated.DeliveredAt)
	assert.Zero(t, updated.DeliveredInputID)
	_, err = ss.PeekPending(ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

func TestLinkStore_BackgroundCompletionRejectsStaleIdentity(t *testing.T) {
	ss, links, projectID := newTestLinkStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	otherParent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, ss, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "call", Depth: 1,
	}))
	require.NoError(t, seedTerminalChild(ctx, ss, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted))
	link, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)

	wrongParent := *link
	wrongParent.ParentID = otherParent.ID
	_, err = links.DeliverBackgroundCompletion(ctx, wrongParent, 1)
	require.Error(t, err)
	stale := *link
	stale.ActivationSeq++
	applied, err := links.DeliverBackgroundCompletion(ctx, stale, 1)
	require.NoError(t, err)
	require.False(t, applied)

	_, err = ss.PeekPending(ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
	_, err = ss.PeekPending(ctx, otherParent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

func TestLinkStore_Finalize(t *testing.T) {
	ss, links, projectID := newTestLinkStore(t)
	ctx := t.Context()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID,
		RootID: parent.ID, Model: "m", TaskCallID: "c1", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	_, err = ss.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages:  []*transcript.Message{{Role: "assistant", Content: "the answer is 42"}},
		},
	)
	require.NoError(t, err)
	link, err := links.Finalize(ctx, childID, false)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.True(t, link.Terminal())
	assert.Equal(t, "the answer is 42", link.Result)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	rec, err := ss.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
	duplicate, err := links.Finalize(ctx, childID, true)
	require.NoError(t, err)
	assert.Nil(t, duplicate)
	preserved, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, link.Result, preserved.Result)
}

func TestLinkStore_ResetRunning(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(
		t,
		seedChildLink(ctx, ss, subagent.Link{ParentID: parent.ID, ChildID: childID, TaskCallID: "c1"}),
	)

	require.NoError(
		t,
		seedTerminalChild(ctx, ss, childID, subagent.StateCompleted, "answer", subagent.OutcomeCompleted),
	)
	deliverOneLink(t, ls, parent.ID, childID)

	require.NoError(t, ls.Resume(ctx, childID))

	link, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateRunning, link.State)
	assert.Zero(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredMsgID)
	// result/outcome are intentionally left stale until the next terminalization.
	assert.Equal(t, "answer", link.Result)
}

func TestLinkStoreRejectsMissingResumeAndKill(t *testing.T) {
	_, links, _ := newTestLinkStore(t)
	require.ErrorContains(t, links.Resume(t.Context(), 999), "not found")
	require.ErrorContains(t, links.Kill(t.Context(), 999), "not found")
}

func TestLinkStore_ListPending(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	c1, _ := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	c2, _ := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, seedChildLink(ctx, ss, subagent.Link{ParentID: parent.ID, ChildID: c1, TaskCallID: "c1"}))
	require.NoError(t, seedChildLink(ctx, ss, subagent.Link{ParentID: parent.ID, ChildID: c2, TaskCallID: "c2"}))

	pending, err := ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	assert.Len(t, pending, 2)

	deliverOneLink(t, ls, parent.ID, c1)

	pending, err = ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, c2, pending[0].ChildID)
}

func TestLinkStore_ListRunningAndUndelivered(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	running, _ := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	done, _ := func() (int64, error) {
		var id int64
		err := ss.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "m",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, seedChildLink(ctx, ss, subagent.Link{
		ParentID: parent.ID, ChildID: running, TaskCallID: "r",
	}))
	require.NoError(t, seedChildLink(ctx, ss, subagent.Link{ParentID: parent.ID, ChildID: done, TaskCallID: "d"}))

	require.NoError(
		t,
		seedTerminalChild(ctx, ss, done, subagent.StateCompleted, "the result", subagent.OutcomeCompleted),
	)

	runningLinks, err := ls.ListRunningChildLinks(ctx)
	require.NoError(t, err)
	require.Len(t, runningLinks, 1)
	assert.Equal(t, running, runningLinks[0].ChildID)

	undelivered, err := ls.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	assert.Equal(t, done, undelivered[0].ChildID)
	// The sl.-aliased join columns carry result/outcome through too.
	assert.Equal(t, "the result", undelivered[0].Result)
	assert.Equal(t, subagent.OutcomeCompleted, undelivered[0].Outcome)

	// Once delivered, it drops out of the undelivered set.
	deliverOneLink(t, ls, parent.ID, done)
	undelivered, err = ls.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	assert.Empty(t, undelivered)
}

func TestFollowUpAcceptedBeforeTerminalBoundaryStaysInSameActivation(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/follow-up-boundary")
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)

	// Keep the accepted child parked so the test can place finalization exactly
	// after the durable enqueue and before any runner promotes the input.
	for i := range maxChildren {
		require.True(t, mgr.runners.tryAdmit(true, int64(10_000+i)))
		defer mgr.runners.release(true, int64(10_000+i))
	}

	require.NoError(t, mgr.SendToChild(ctx, childID, "one more question"))
	finalizeTestChild(ctx, t, mgr, childID)

	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.False(t, link.Terminal(), "accepted input wins the activation boundary")

	pending, err := mgr.store.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "one more question", pending.RawContent)
}

func TestTerminalChildDeliversPreviousOutcomeBeforeRearm(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, t.TempDir())
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)

	require.NoError(
		t,
		seedTerminalChild(ctx, projects, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted),
	)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	mgr.startWake()

	require.NoError(t, mgr.SendToChild(ctx, childID, "follow-up after completion"))

	require.Eventually(t, func() bool {
		link, linkErr := mgr.links.GetLink(ctx, childID)
		if linkErr != nil || link == nil || link.State != subagent.StateRunning || link.DeliveredAt != 0 {
			return false
		}

		messages, msgErr := mgr.store.LoadActiveMessages(ctx, parent.ID)
		if msgErr != nil {
			return false
		}
		for _, message := range messages {
			if message.Role == "user" &&
				containsAll(message.Content, "<subagent_completion>", "first outcome", "outcome: completed") {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	mgr.Shutdown(3 * time.Second)
}

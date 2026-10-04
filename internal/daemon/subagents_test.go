package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
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
	h := newHarness(t, harnessOptions{dbPath: dbPath, respond: trivialRespond})
	sessions := h.store
	links := h.links
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
	h.mgr.links = handoff
	manager := h.mgr

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

func TestFollowUpAcceptedBeforeTerminalBoundaryStaysInSameActivation(t *testing.T) {
	ctx := context.Background()
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	projects := testHarness.store
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
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	projects := testHarness.store
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

	testHarness.waitUntil("TestTerminalChildDeliversPreviousOutcomeBeforeRearm", func() bool {
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
	})

	mgr.Shutdown(3 * time.Second)
}

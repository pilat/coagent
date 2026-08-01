package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// TestFinalizeChild_IncompleteWhenNoFinalAnswer: a child that ran out of
// iterations with its last message a tool call (no final answer) terminalizes as
// `incomplete`, not a silent `completed`, and the parent sees that explicitly.
func TestFinalizeChild_IncompleteWhenNoFinalAnswer(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx

	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		ctx,
		h.projectID,
		parent.ID,
		parent.ID,
		"general",
		"fake-model",
		"",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, SubagentLink{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// The child's last message is a tool call — it stopped mid-tool / hit its cap.
	toolCalls, err := json.Marshal([]llmwire.ToolCall{{ID: "x", Name: "bash", Arguments: []byte(`{}`)}})
	require.NoError(t, err)
	_, err = h.sessStore.InsertMessage(ctx, childID, &sessionstore.StoredMessage{
		Role: llmwire.RoleAssistant, ToolCalls: toolCalls,
	})
	require.NoError(t, err)
	// Max-iterations persists status "error" with errored == false.
	require.NoError(t, h.sessStore.UpdateSessionIteration(
		ctx, childID, 12, sessionstore.SessionStatusError,
	))

	h.mgr.finalizeChild(ctx, childID, false, false)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, LinkOutcomeIncomplete, link.Outcome, "no final answer → incomplete")
	assert.Contains(t, link.Result, "without a final answer")
	assert.Contains(t, link.Result, "12", "result note carries the iteration count")
	assert.Equal(t, LinkStateError, link.State, "max-iterations keeps the state=error lifecycle value")

	// The parent receives the explicit incomplete outcome, never a masked completed.
	h.waitForDelivery(childID)
	h.mgr.waitIdle(parent.ID)
	assert.Contains(t, lastToolResultContent(h.parentMessages(parent.ID), "subagent_event"), "incomplete")
}

// TestCascadeKill_BackgroundDescendant: killing a parent stops every non-terminal
// descendant — blocking AND background — across the depth bound, and each killed
// unfinished descendant produces exactly one WARN audit line.
func TestCascadeKill_BackgroundDescendant(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			<-release // hang until kill cancels the loop ctx

			return &llmwire.Response{Text: "unreached"}
		}

		return &llmwire.Response{Text: "idle"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	ctx := h.ctx

	root, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	child, err := h.mgr.Spawn(ctx, spawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })

	grandchild, err := h.mgr.Spawn(ctx, spawnRequest{
		ParentID: child.ChildID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background grandchild running", func() bool { return h.mgr.HasActiveLoop(grandchild.ChildID) })

	// Capture WARN audit lines emitted during the cascade kill.
	core, logs := observer.New(zap.WarnLevel)
	killCtx := logger.ToContext(ctx, zap.New(core))

	require.NoError(t, h.mgr.Kill(killCtx, root.ID))

	h.waitUntil("both descendants gone", func() bool {
		return !h.mgr.HasActiveLoop(child.ChildID) && !h.mgr.HasActiveLoop(grandchild.ChildID)
	})

	for _, id := range []int64{child.ChildID, grandchild.ChildID} {
		rec, gerr := h.sessStore.GetSession(ctx, id)
		require.NoError(t, gerr)
		assert.NotNil(t, rec.KilledAt, "descendant %d is killed with its tree", id)
	}

	assert.Len(t, logs.FilterMessage("cascade_killed_descendant").All(), 2,
		"one WARN per killed unfinished descendant")
}

// TestCascadeKill_RemovesChildSchedules: cascade-killing a background child
// deletes its schedules too — killSubagent owns schedule teardown, same as the
// direct Kill path.
func TestCascadeKill_RemovesChildSchedules(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			<-release // hang until kill cancels the loop ctx

			return &llmwire.Response{Text: "unreached"}
		}

		return &llmwire.Response{Text: "idle"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	ctx := h.ctx

	root, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	child, err := h.mgr.Spawn(ctx, spawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "", &oneShot, "child one-shot", false)
	require.NoError(t, err)
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "0 9 * * *", nil, "child cron", false)
	require.NoError(t, err)

	require.NoError(t, h.mgr.Kill(ctx, root.ID))
	h.waitUntil("child gone", func() bool { return !h.mgr.HasActiveLoop(child.ChildID) })

	remaining, err := h.schedStore.ListSchedules(ctx, child.ChildID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cascade-killed child's schedules are removed")
}

// TestCascadeKill_CompletedUndeliveredSurvives: a background descendant that
// already completed (terminal, undelivered) is NOT re-marked killed by the
// cascade — its stored result/outcome survive.
func TestCascadeKill_CompletedUndeliveredSurvives(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx

	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		ctx,
		h.projectID,
		parent.ID,
		parent.ID,
		"general",
		"fake-model",
		"",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, SubagentLink{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))
	require.NoError(t, h.links.MarkLinkTerminal(
		ctx, childID, LinkStateCompleted, "the result", LinkOutcomeCompleted,
	))
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	h.mgr.cascadeKillChildren(ctx, parent.ID, 0, time.Time{})

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, LinkStateCompleted, link.State, "completed-but-undelivered child not re-marked killed")
	assert.Equal(t, LinkOutcomeCompleted, link.Outcome)
	assert.Equal(t, "the result", link.Result, "its stored result survives the cascade")
}

// TestDrainQueue_SkipsKilledChild: a queued child cascade-killed before it ran is
// never launched by a subsequent drainQueue.
func TestDrainQueue_SkipsKilledChild(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx

	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		ctx,
		h.projectID,
		parent.ID,
		parent.ID,
		"general",
		"fake-model",
		"",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, SubagentLink{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// Park the child, then kill it before any runner picks it up.
	h.mgr.enqueueChild(ctx, childID, parent.ID, "/tmp", h.projectID)
	h.mgr.killSubagent(ctx, childID, time.Time{})

	h.mgr.drainQueue(ctx)

	assert.False(t, h.mgr.HasActiveLoop(childID), "a killed queued child is never launched")
	assert.Equal(t, 0, h.queueLen(), "the killed entry is purged from the queue")
}

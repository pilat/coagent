package daemon

import (
	"database/sql"
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
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
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

	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
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
	require.NoError(t, seedChildLink(ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// The child's last message is a tool call — it stopped mid-tool / hit its cap.
	toolCalls, err := json.Marshal([]llmwire.ToolCall{{ID: "x", Name: "bash", Arguments: []byte(`{}`)}})
	require.NoError(t, err)
	_, err = h.sessStore.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: toolCalls,
	}}})
	require.NoError(t, err)
	// Max-iterations persists status "error" with errored == false.
	require.NoError(t, func() error {
		iteration := 12
		status := sessionstore.SessionStatusError
		_, err := h.sessStore.Commit(
			ctx,
			sessionstore.Commit{
				SessionID: childID,
				State:     sessionstore.StatePatch{Iteration: &iteration, Status: &status},
			},
		)
		return err
	}())

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeIncomplete, link.Outcome, "no final answer → incomplete")
	assert.Contains(t, link.Result, "without a final answer")
	assert.Contains(t, link.Result, "12", "result note carries the iteration count")
	assert.Equal(t, subagent.StateError, link.State, "max-iterations keeps the state=error lifecycle value")

	// The parent receives the explicit incomplete outcome, never a masked completed.
	h.waitForDelivery(childID)
	h.mgr.waitIdle(parent.ID)
	assert.Contains(t, lastSubagentCompletion(h.parentMessages(parent.ID), childID), "outcome: incomplete")
}

// TestCascadeKill_BackgroundDescendant: killing a parent stops every non-terminal
// descendant — blocking AND background — across the depth bound, and each killed
// unfinished descendant produces exactly one WARN audit line.
func TestCascadeKill_BackgroundDescendant(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			entered <- struct{}{}
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

	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })
	h.waitUntil("background child entered model call", func() bool { return len(entered) >= 1 })

	h.startInboxWake()
	grandchild, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: child.ChildID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background grandchild running", func() bool { return h.mgr.HasActiveLoop(grandchild.ChildID) })
	h.waitUntil("background grandchild entered model call", func() bool { return len(entered) >= 2 })
	childInput, err := h.sessStore.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  child.ChildID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "pending",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	grandchildInput, err := h.sessStore.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  grandchild.ChildID,
			Source:     sessionstore.InputSourceSubagent,
			Content:    "complete",
			Attributes: nil,
		},
	)
	require.NoError(t, err)

	// Capture WARN audit lines emitted during the cascade kill.
	core, logs := observer.New(zap.WarnLevel)
	killCtx := logger.ToContext(ctx, zap.New(core))

	require.NoError(t, h.mgr.sendToSession(killCtx, root.ID, "/kill"))

	h.waitUntil("both descendants gone", func() bool {
		return !h.mgr.HasActiveLoop(child.ChildID) && !h.mgr.HasActiveLoop(grandchild.ChildID)
	})

	for _, id := range []int64{child.ChildID, grandchild.ChildID} {
		rec, gerr := h.sessStore.GetSession(ctx, id)
		require.NoError(t, gerr)
		assert.NotNil(t, rec.KilledAt, "descendant %d is killed with its tree", id)
	}
	for _, inputID := range []int64{childInput.Input.ID, grandchildInput.Input.ID} {
		var state string
		require.NoError(t, h.db.QueryRowContext(ctx,
			`SELECT state FROM session_inbox WHERE id = ?`, inputID,
		).Scan(&state))
		assert.Equal(t, string(sessionstore.InputStateCancelled), state)
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

	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "", &oneShot, "child one-shot", false)
	require.NoError(t, err)
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "0 9 * * *", nil, "child cron", false)
	require.NoError(t, err)

	require.NoError(t, h.mgr.sendToSession(ctx, root.ID, "/kill"))
	h.waitUntil("child gone", func() bool { return !h.mgr.HasActiveLoop(child.ChildID) })

	remaining, err := h.schedStore.ListSchedules(ctx, child.ChildID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cascade-killed child's schedules are removed")
}

func TestCascadeKill_KilledTreeSuppressesTerminalBackgroundCompletion(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

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
		ParentID: parent.ID, ChildID: childID, TaskCallID: "background",
	}))
	require.NoError(
		t,
		seedTerminalChild(h.ctx, h.sessStore, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted),
	)
	require.NoError(
		t,
		h.sessStore.WithTx(
			h.ctx,
			func(tx *sql.Tx) error { return sessionstore.MarkSessionKilledTx(h.ctx, tx, parent.ID) },
		),
	)

	h.mgr.killDescendants(h.ctx, parent.ID, 0)

	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, "done", link.Result)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Positive(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredInputID)
	assert.Zero(t, link.DeliveredMsgID)
	_, err = h.sessStore.PeekPending(h.ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

// TestDrainQueue_SkipsKilledChild: a queued child cascade-killed before it ran is
// never launched by a subsequent drainQueue.
func TestDrainQueue_SkipsKilledChild(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx

	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
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
	require.NoError(t, seedChildLink(ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// Park the child, then kill it before any runner picks it up.
	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent.ID, child: true})
	h.mgr.killSubagent(ctx, childID)

	h.mgr.drain(ctx)

	assert.False(t, h.mgr.HasActiveLoop(childID), "a killed queued child is never launched")
	assert.Equal(t, 0, h.queueLen(), "the killed entry is purged from the queue")
}

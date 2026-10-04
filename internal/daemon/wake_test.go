package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestProcessInputRearmsCompletedChildAfterPriorOutcomeHandoff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, t.TempDir())
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)
	for i := range maxChildren {
		require.True(t, mgr.runners.tryAdmit(true, int64(20_000+i)))
		defer mgr.runners.release(true, int64(20_000+i))
	}
	require.NoError(
		t,
		seedTerminalChild(ctx, projects, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted),
	)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	mgr.startWake()
	processInput, err := mgr.store.Enqueue(
		ctx, sessionstore.Input{
			SessionID:  childID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "<process_completion>late process</process_completion>",
			Attributes: map[string]any{"process_id": "late-process"},
		},
	)
	require.NoError(t, err)
	require.NoError(t, mgr.inputReady(ctx, childID))
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateRunning, link.State)
	assert.Equal(t, int64(2), link.ActivationSeq)
	assert.False(t, link.Blocking)
	assert.Zero(t, link.DeliveredAt)
	pending, err := mgr.store.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, processInput.Input.ID, pending.ID)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
	h.waitUntil("child rearmed", func() bool {
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

func TestProcessInputDoesNotRearmCompletedChildAfterStop(t *testing.T) {
	cases := []struct {
		name      string
		stopChild bool
	}{
		{name: "root stops"},
		{name: "direct child stop", stopChild: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assertProcessInputDoesNotRearmAfterStop(t, tt.stopChild)
		})
	}
}

func assertProcessInputDoesNotRearmAfterStop(t *testing.T, stopChild bool) {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, "/tmp/process-stop-rearm")
	root, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, root.ID)
	require.NoError(
		t,
		seedTerminalChild(ctx, projects, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted),
	)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	won, err := mgr.links.DeliverBackgroundCompletion(ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)
	_, err = mgr.store.Enqueue(
		ctx, sessionstore.Input{
			SessionID:  childID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "<process_completion>late process</process_completion>",
			Attributes: map[string]any{"process_id": "late-process"},
		},
	)
	require.NoError(t, err)
	unlock, err := mgr.lockSessionTree(ctx, root.ID)
	require.NoError(t, err)
	ready := make(chan error, 1)
	go func() { ready <- mgr.inputReady(ctx, childID) }()
	stopID := root.ID
	if stopChild {
		stopID = childID
	}
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, stopID, sessionstore.SessionStatusStopped))
	unlock()
	require.NoError(t, <-ready)
	link, err = mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, int64(1), link.ActivationSeq)
	pending, err := mgr.store.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
}

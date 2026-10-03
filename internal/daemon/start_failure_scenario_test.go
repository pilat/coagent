package daemon

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestScenario_StartFailureParksWithoutConsumingInput(t *testing.T) {
	h := newModelAwareHarness(
		t,
		[]string{"removed-model", "working-model"},
		func(string, []llmwire.Message) *llmwire.Response {
			return &llmwire.Response{Text: "done"}
		},
	)
	defer h.shutdown()
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "removed-model", nil)
	require.NoError(t, err)
	h.waitUntil("first answer", func() bool { return countAssistantReplies(h.parentMessages(id)) == 2 })
	h.mgr.waitIdle(id)
	h.mgr.build.Config.UnifiedConfig.Models = h.mgr.build.Config.UnifiedConfig.Models[1:]
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "keep this input"))
	h.waitUntil("failure observed", func() bool {
		record, err := h.sessStore.GetSession(h.ctx, id)
		return err == nil && record.Status == sessionstore.SessionStatusError
	})
	h.mgr.waitIdle(id)
	record, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusError, record.Status)
	pending, err := h.sessStore.PeekPending(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	require.NoError(t, h.mgr.SetModel(h.ctx, id, "working-model", ""))
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "retry now"))
	h.waitUntil("retry consumes preserved work", func() bool {
		_, err := h.sessStore.PeekPending(h.ctx, id)
		return errors.Is(err, sessionstore.ErrNoPendingInput)
	})
	h.mgr.waitIdle(id)
	assert.True(t, hasUserContaining(h.parentMessages(id), "keep this input"))
}

func TestScenario_RepeatedStartFailureCreatesOneOutput(t *testing.T) {
	h := newModelAwareHarness(t, []string{"working-model"}, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "done"}
	})
	defer h.shutdown()
	record, err := h.sessStore.CreateSession(h.ctx, h.projectID, "removed-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "test-manager",
	})
	require.NoError(t, err)
	_, err = h.sessStore.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: record.ID, Source: sessionstore.InputSourceUser, Content: "work"},
	)
	require.NoError(t, err)
	notifications := h.mgr.bus.Subscribe(record.ID)
	defer h.mgr.bus.Unsubscribe(record.ID, notifications)
	for range 100 {
		h.mgr.reportSessionUnstarted(
			h.ctx,
			record.ID,
			errors.New("model removed-model not found in config"),
		)
	}
	status, err := h.sessStore.OutputQueueStatus(h.ctx, "test-manager")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
	var notices int
	for len(notifications) > 0 {
		if (<-notifications).Type == sessionevent.NotifyMessage {
			notices++
		}
	}
	assert.Equal(t, 1, notices)
}

func TestScenario_StartFailureRestartKeepsOneReceipt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	respond := func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "done"}
	}
	first := newModelAwareHarnessAtDB(t, dbPath, []string{"working-model"}, respond)
	t.Cleanup(first.shutdown)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "removed-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "test-manager",
	})
	require.NoError(t, err)
	input, err := first.sessStore.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "preserved work"},
	)
	require.NoError(t, err)
	first.shutdown()

	for range 2 {
		h := newModelAwareHarnessAtDB(t, dbPath, []string{"working-model"}, respond)
		t.Cleanup(h.shutdown)
		h.startInboxWake()
		h.mgr.resumeAfterRestart(h.ctx)
		h.waitUntil("failed recovery parked", func() bool {
			record, err := h.sessStore.GetSession(h.ctx, root.ID)
			return err == nil && record.Status == sessionstore.SessionStatusError && !h.mgr.HasActiveLoop(root.ID)
		})
		h.shutdown()
		status, err := h.sessStore.OutputQueueStatus(h.ctx, "test-manager")
		require.NoError(t, err)
		assert.Equal(t, 1, status.Pending)
		pending, err := h.sessStore.PeekPending(h.ctx, root.ID)
		require.NoError(t, err)
		assert.Equal(t, input.Input.ID, pending.ID)
	}
}

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type unavailableModelFactory struct {
	session.Factory
	unavailable atomic.Bool
	failures    atomic.Int64
}

func (f *unavailableModelFactory) Create(ctx context.Context, opts session.CreateOptions) (session.Service, error) {
	if !opts.TranscriptOnly && opts.Model == "removed-model" && f.unavailable.Load() {
		f.failures.Add(1)
		return nil, errors.New("model removed-model not found in config")
	}
	return f.Factory.Create(ctx, opts)
}

func TestScenario_StartFailureParksWithoutConsumingInput(t *testing.T) {
	h := newModelAwareHarness(
		t,
		[]string{"removed-model", "working-model"},
		func(string, []llmwire.Message) *llmwire.Response {
			return &llmwire.Response{Text: "done"}
		},
	)
	defer h.shutdown()
	factory := &unavailableModelFactory{Factory: h.mgr.factory}
	h.mgr.factory = factory
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "removed-model", nil)
	require.NoError(t, err)
	h.waitUntil("first answer", func() bool { return countAssistantReplies(h.parentMessages(id)) == 2 })
	h.mgr.waitIdle(id)
	factory.unavailable.Store(true)
	require.NoError(t, h.mgr.SendToSession(h.ctx, id, "keep this input"))
	h.waitUntil("failure observed", func() bool { return factory.failures.Load() > 0 })
	h.mgr.waitIdle(id)
	assert.Equal(t, int64(1), factory.failures.Load(), "failed creation must not restart itself")
	record, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusError, record.Status)
	pending, err := h.sessStore.PeekPending(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	require.NoError(t, h.mgr.SetModel(h.ctx, id, "working-model", ""))
	require.NoError(t, h.mgr.SendToSession(h.ctx, id, "retry now"))
	h.waitUntil("retry consumes preserved work", func() bool {
		_, err := h.sessStore.PeekPending(h.ctx, id)
		return errors.Is(err, sessionstore.ErrNoPendingInput)
	})
	h.mgr.waitIdle(id)
	assert.Equal(t, int64(1), factory.failures.Load())
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
	_, err = h.sessStore.EnqueueInput(h.ctx, record.ID, sessionstore.InputSourceUser, "work")
	require.NoError(t, err)
	notices := 0
	for range 100 {
		h.mgr.reportSessionUnstarted(
			h.ctx,
			record.ID,
			func(sessionevent.Notification) { notices++ },
			errors.New("model removed-model not found in config"),
		)
	}
	status, err := h.sessStore.OutputQueueStatus(h.ctx, "test-manager")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
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
	input, err := first.sessStore.EnqueueInput(first.ctx, root.ID, sessionstore.InputSourceUser, "preserved work")
	require.NoError(t, err)
	first.shutdown()

	for range 2 {
		h := newModelAwareHarnessAtDB(t, dbPath, []string{"working-model"}, respond)
		t.Cleanup(h.shutdown)
		factory := &unavailableModelFactory{Factory: h.mgr.factory}
		factory.unavailable.Store(true)
		h.mgr.factory = factory
		h.mgr.sweep(h.ctx)
		h.waitUntil("failed recovery parked", func() bool {
			return factory.failures.Load() > 0 && !h.mgr.HasActiveLoop(root.ID)
		})
		h.shutdown()
		assert.Equal(t, int64(1), factory.failures.Load())
		status, err := h.sessStore.OutputQueueStatus(h.ctx, "test-manager")
		require.NoError(t, err)
		assert.Equal(t, 1, status.Pending)
		pending, err := h.sessStore.PeekPending(h.ctx, root.ID)
		require.NoError(t, err)
		assert.Equal(t, input.ID, pending.ID)
	}
}

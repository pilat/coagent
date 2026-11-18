package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrchestratorUnreadSignalCoversFollowUpOutcome(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	host := orchestratorHost{s: h.mgr}
	_, _, err := host.SendToSession(h.ctx, watcher, target, "first")
	require.NoError(t, err)
	assertAnswerSignal(t, h, watcher, target)
	waitSignalTurn(t, h, watcher, "done")
	before := h.countOutbox(target, func(content string) bool { return content == "done" })
	_, _, err = host.SendToSession(h.ctx, watcher, target, "follow-up before read")
	require.NoError(t, err)
	h.waitUntil("follow-up answer", func() bool {
		return h.countOutbox(target, func(content string) bool { return content == "done" }) > before
	})
	waitSignalTurn(t, h, target, "done")
	assert.Equal(t, 1, signalRows(t, h, watcher, target))
	_, err = host.ReadSession(h.ctx, watcher, target, false)
	require.NoError(t, err)
	entries, _, err := host.ListSessions(h.ctx, watcher, "")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.ID == target {
			assert.False(t, entry.Armed)
			assert.False(t, entry.Signalled)
		}
	}

	wake, err := h.store.HasBackgroundWakeSource(h.ctx, watcher)
	require.NoError(t, err)
	assert.False(t, wake)
	obligation, err := h.store.HasBackgroundObligationByRoot(h.ctx, watcher)
	require.NoError(t, err)
	assert.False(t, obligation)
}

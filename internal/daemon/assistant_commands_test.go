package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/assistant"
)

func TestAssistantRefusalAfterCommandConsumer(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	input, err := h.mgr.enqueueUserSessionInput(h.ctx, root.ID, killCommand)
	require.NoError(t, err)
	deps, err := h.mgr.assistantDeps(h.ctx, root.ID)
	require.NoError(t, err)
	require.NoError(t, assistant.Run(h.ctx, deps, root.ID))
	require.NoError(t, h.mgr.kill(h.ctx, input))
	require.NoError(t, h.mgr.kill(h.ctx, input))
	var state, reason string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state, resolution_reason FROM session_inbox WHERE id = ?`,
		input.ID).Scan(&state, &reason))
	assert.Equal(t, "rejected", state)
	assert.Equal(t, "assistant command refused", reason)
	assert.Equal(t, 1, assistantSayCount(t, h, root.ID, "This command is unavailable in the Assistant topic."))
}

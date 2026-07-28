package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSessionSettlesTheEffortOnItsModel: a fresh session names no level, so it
// must start on its model's default — anything else is a level nobody chose.
func TestNewSessionSettlesTheEffortOnItsModel(t *testing.T) {
	provider := newSpawnEffortProvider(t)
	h := newSpawnEffortHarness(t, provider.url)

	defer h.shutdown()

	id, err := h.mgr.Send(h.ctx, h.projectID, "work", "parent-model", nil)
	require.NoError(t, err)
	h.waitUntil("answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 1
	})
	h.mgr.waitIdle(id)

	rec, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"the record is all a later run reads, so it must carry the model's default")
	assert.Equal(t, "high", provider.effortFor("parent-model"),
		"a fresh session must ask for its model's default, not a clamped stand-in")
}

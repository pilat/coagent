package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/tool"
)

// A verdict resolves the exact call that suspended, and clears the ledger entry
// so the next session build no longer blocks on it.
func TestPrepareSessionInputs_VerdictResolvesTheStagedCall(t *testing.T) {
	ctx := context.Background()
	mgr, _, store := newTestManager(t)
	pid := testProject(t, store, "/tmp/test")

	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	mgr.staged.stage(rec.ID, "call-7", tool.IDSetProvider)
	require.True(t, mgr.staged.has(rec.ID))

	rs := newInputsRunner(pid, []queuedSessionInput{asyncSessionInput{value: pendingCallResultInput{
		Call:    session.PendingToolCall{ID: "call-7", Name: tool.IDSetProvider},
		Content: "applied",
	}}})

	notifs, err := mgr.prepareSessionInputs(ctx, rec.ID, rs, &mockSession{
		pendingCalls: []session.PendingToolCall{{ID: "call-7", Name: tool.IDSetProvider}},
	})
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	assert.False(t, mgr.staged.has(rec.ID), "the ledger entry is cleared once the result is injected")
}

func TestStagedCalls_Lifecycle(t *testing.T) {
	c := newStagedCalls()

	assert.False(t, c.has(1))
	assert.Nil(t, c.forSession(1))

	c.stage(1, "a", tool.IDSetProvider)
	c.stage(1, "b", tool.IDRequestSecret)

	assert.True(t, c.has(1))
	assert.Equal(t, map[string]string{"a": tool.IDSetProvider, "b": tool.IDRequestSecret}, c.forSession(1))

	// The copy is a copy: mutating it must not reach the ledger.
	c.forSession(1)["a"] = "tampered"
	assert.Equal(t, tool.IDSetProvider, c.forSession(1)["a"])

	c.resolve(1, "a")
	assert.Equal(t, map[string]string{"b": tool.IDRequestSecret}, c.forSession(1))

	c.resolve(1, "b")
	assert.False(t, c.has(1))
	assert.Nil(t, c.forSession(1))

	c.resolve(1, "gone") // resolving twice is not an error
}

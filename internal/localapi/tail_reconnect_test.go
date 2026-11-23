package localapi

import (
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestTailOutput_ReconnectWithTheReplacedIDContinuesOnTheSuccessor(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	before := f.tail(t, h, id, 0)
	_ = nextOutput(t, before)
	lastID := f.append(t, id, "before clear")
	f.wake(id)
	last := nextOutput(t, before)
	require.Equal(t, lastID, last.GetId())
	require.NoError(t, before.Close())
	root, commit, err := f.store.ReplaceManagerRoot(t.Context(), id, "test", "/project")
	require.NoError(t, err)
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	afterID := f.append(t, root.ID, "after clear")
	resumed := f.tail(t, h, id, last.GetId())
	replaced := nextOutput(t, resumed)
	assert.Equal(t, commit.OutputID, replaced.GetId())
	assert.Equal(t, root.ID, replaced.GetSessionId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED, replaced.GetType())
	after := nextOutput(t, resumed)
	assert.Equal(t, afterID, after.GetId())
	assert.Equal(t, root.ID, after.GetSessionId())
	assert.Greater(t, replaced.GetId(), last.GetId())
	assert.Greater(t, after.GetId(), replaced.GetId())
	liveID := f.append(t, root.ID, "live")
	f.wake(root.ID)
	live := nextOutput(t, resumed)
	assert.Equal(t, liveID, live.GetId())
	assert.Equal(t, root.ID, live.GetSessionId())
}

func TestTailOutput_ReconnectWithTheReplacedIDAndASuccessorCursor(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	before := f.tail(t, h, id, 0)
	_ = nextOutput(t, before)
	root, replacement, err := f.store.ReplaceManagerRoot(t.Context(), id, "test", "/project")
	require.NoError(t, err)
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	f.wake(id)
	row := nextOutput(t, before)
	require.Equal(t, replacement.OutputID, row.GetId())
	require.Equal(t, root.ID, row.GetSessionId())
	seenID := f.append(t, root.ID, "already received")
	f.wake(root.ID)
	seen := nextOutput(t, before)
	require.Equal(t, seenID, seen.GetId())
	require.Equal(t, root.ID, seen.GetSessionId())
	require.NoError(t, before.Close())
	unread := []int64{f.append(t, root.ID, "unread one"), f.append(t, root.ID, "unread two")}
	resumed := f.tail(t, h, id, seen.GetId())
	for _, wantID := range unread {
		output := nextOutput(t, resumed)
		require.Equal(t, wantID, output.GetId())
		assert.Equal(t, root.ID, output.GetSessionId())
	}
	liveID := f.append(t, root.ID, "live after reconnect")
	f.wake(root.ID)
	live := nextOutput(t, resumed)
	assert.Equal(t, liveID, live.GetId())
	assert.Equal(t, root.ID, live.GetSessionId())
	assert.Equal(t, "live after reconnect", live.GetContent())
}

func TestTailOutput_SkipsARowTheReplacedRootCommitsAfterItsReplacement(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	limit := 1.0
	armed, err := f.store.Arm(t.Context(), budget.Mutation{
		RootSessionID: id, ToolCallID: "tail-budget", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	before := f.tail(t, h, id, 0)
	_ = nextOutput(t, before)
	last := nextOutput(t, before)
	require.Equal(t, "Budget armed", last.GetContent())
	require.NoError(t, before.Close())
	pending := make([]int64, 0, 101)
	for i := range 101 {
		pending = append(pending, f.append(t, id, fmt.Sprintf("before clear %d", i)))
	}
	input, err := f.store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: id, Source: sessionstore.InputSourceUser, Content: "/clear",
	})
	require.NoError(t, err)
	root, replaced, err := f.store.ReplaceManagerRootForInput(t.Context(), id, input.Input.ID, "test", "/project")
	require.NoError(t, err)
	_, late, err := f.store.FireBudget(t.Context(), id, armed.Generation, "cost", 2, "late old-root checkpoint")
	require.NoError(t, err)
	require.Greater(t, late.OutputID, replaced.OutputID)
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	afterID := f.append(t, root.ID, "after clear")
	closed, err := f.store.MarkSessionKilledWithOutput(t.Context(), root.ID, 0)
	require.NoError(t, err)
	resumed := f.tail(t, h, id, last.GetId())
	for _, wantID := range pending {
		row := nextOutput(t, resumed)
		require.Equal(t, wantID, row.GetId())
		assert.Equal(t, id, row.GetSessionId())
	}
	first := nextOutput(t, resumed)
	require.Equal(t, replaced.OutputID, first.GetId(), "late old-root output must not skip successor rows")
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED, first.GetType())
	cleared := nextOutput(t, resumed)
	assert.Equal(t, "Session cleared.", cleared.GetContent())
	after := nextOutput(t, resumed)
	assert.Equal(t, afterID, after.GetId())
	lastRow := nextOutput(t, resumed)
	assert.Equal(t, closed.OutputID, lastRow.GetId())
	for _, row := range []*coagentv1.OutputRow{first, cleared, after, lastRow} {
		assert.Equal(t, root.ID, row.GetSessionId())
		assert.NotEqual(t, late.OutputID, row.GetId())
	}
	assert.Less(t, first.GetId(), cleared.GetId())
	assert.Less(t, cleared.GetId(), after.GetId())
	assert.Less(t, after.GetId(), lastRow.GetId())
	_, err = resumed.Receive()
	require.ErrorIs(t, err, io.EOF)
}

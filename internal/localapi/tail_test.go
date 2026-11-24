package localapi

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestTailOutput_ResumesAfterRestartWithoutGapOrRepeat(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	a := f.start(t)
	id := f.root(t, a, "alpha")
	firstID := f.append(t, id, "before restart")
	before := f.tail(t, a, id, 0)
	opened := nextOutput(t, before)
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_OPENED, opened.GetType())
	first := nextOutput(t, before)
	assert.Equal(t, firstID, first.GetId())
	a.close(t)
	_, err := before.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	offlineOne := f.append(t, id, "offline one")
	offlineTwo := f.append(t, id, "offline two")
	b := f.start(t)
	after := f.tail(t, b, id, first.GetId())
	one := nextOutput(t, after)
	two := nextOutput(t, after)
	liveID := f.append(t, id, "live")
	f.wake(id)
	live := nextOutput(t, after)
	assert.Equal(t, []int64{offlineOne, offlineTwo, liveID}, []int64{one.GetId(), two.GetId(), live.GetId()})
	assert.Equal(
		t,
		[]string{"offline one", "offline two", "live"},
		[]string{one.GetContent(), two.GetContent(), live.GetContent()},
	)
	assert.Less(t, opened.GetId(), first.GetId())
	assert.Less(t, first.GetId(), one.GetId())
	assert.Less(t, one.GetId(), two.GetId())
	assert.Less(t, two.GetId(), live.GetId())
	rows, err := f.store.ChatOutputs(t.Context(), id, 0, 100)
	require.NoError(t, err)
	wantIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		wantIDs = append(wantIDs, row.ID)
	}
	assert.Equal(t, wantIDs, []int64{opened.GetId(), first.GetId(), one.GetId(), two.GetId(), live.GetId()})
}

func TestTailOutput_FollowsClearReplacement(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	var armed atomic.Bool
	checked := make(chan struct{})
	resume := make(chan struct{}, 1)
	defer close(resume)
	gateSuccessor(t, f, func(ctx context.Context, next int64) {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		assert.Zero(t, next)
		close(checked)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	h := f.start(t)
	id := f.root(t, h, "alpha")
	stream := f.tail(t, h, id, 0)
	opened := nextOutput(t, stream)
	armed.Store(true)
	f.wake(id)
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not check the old root's successor")
	}
	root, commit, err := f.store.ReplaceRoot(t.Context(), id, "test", "/project")
	require.NoError(t, err)
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	resume <- struct{}{}
	f.wake(id)
	replaced := nextOutput(t, stream)
	assert.Equal(t, commit.OutputID, replaced.GetId())
	assert.Equal(t, root.ID, replaced.GetSessionId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED, replaced.GetType())
	assert.Greater(t, replaced.GetId(), opened.GetId())
	liveID := f.append(t, root.ID, "after clear")
	f.wake(root.ID)
	live := nextOutput(t, stream)
	assert.Equal(t, liveID, live.GetId())
	assert.Equal(t, root.ID, live.GetSessionId())
	assert.Equal(t, "after clear", live.GetContent())
}

func TestTailOutput_EndsAfterKilledRootDrains(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	stream := f.tail(t, h, id, 0)
	_ = nextOutput(t, stream)
	messageID := f.append(t, id, "last message")
	closed, err := f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	f.wake(id)
	message := nextOutput(t, stream)
	last := nextOutput(t, stream)
	assert.Equal(t, messageID, message.GetId())
	assert.Equal(t, closed.OutputID, last.GetId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_CLOSED, last.GetType())
	_, err = stream.Receive()
	require.ErrorIs(t, err, io.EOF)
}

func TestTailOutput_CarriesRuntimeState(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	stream := f.tail(t, h, id, 0)
	_ = nextOutput(t, stream)
	f.bus.Publish(id, true, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateRunning,
	})
	event, err := stream.Receive()
	require.NoError(t, err)
	require.NotNil(t, event.GetState())
	assert.Equal(t, id, event.GetState().GetSessionId())
	assert.Equal(t, coagentv1.RuntimeState_RUNTIME_STATE_RUNNING, event.GetState().GetState())
}

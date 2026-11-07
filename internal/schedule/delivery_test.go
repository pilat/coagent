package schedule

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func newDeliveryStore(t *testing.T) (Store, *sessionstore.Store, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "delivery.db")
	db, err := migrate.OpenDB(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	sessions := sessionstore.NewStore(db)
	return NewStore(db, sessions), sessions, seedOneShotSession(t.Context(), t, db)
}

func TestDeliverOneShotCancelledAfterListingDeliversNothing(t *testing.T) {
	t.Parallel()
	store, sessions, root := newDeliveryStore(t)
	now := time.Now().UTC()
	_, err := store.AddSchedule(t.Context(), root, "", &now, "cancelled", false, ScheduleMetadata{})
	require.NoError(t, err)
	due, err := store.ListDueSchedules(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.NoError(t, store.RemoveSchedule(t.Context(), due[0].id))
	applied, err := store.DeliverOneShot(t.Context(), due[0], due[0].inputMessage, nil)
	require.NoError(t, err)
	assert.False(t, applied)
	inputs, err := sessions.ListPending(t.Context(), root)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	assert.Empty(t, sessions.TakeWoken())
}

func TestDeliverCronTwiceInOneMinuteEnqueuesOnce(t *testing.T) {
	t.Parallel()
	store, sessions, root := newDeliveryStore(t)
	sched, err := store.AddSchedule(t.Context(), root, "* * * * *", nil, "tick", false, ScheduleMetadata{})
	require.NoError(t, err)
	minute := time.Now().UTC().Truncate(time.Minute)
	applied, err := store.DeliverCron(t.Context(), sched, "tick", nil, minute, minute.Add(time.Second))
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, []int64{root}, sessions.TakeWoken())
	applied, err = store.DeliverCron(t.Context(), sched, "tick", nil, minute, minute.Add(40*time.Second))
	require.NoError(t, err)
	assert.False(t, applied)
	inputs, err := sessions.ListPending(t.Context(), root)
	require.NoError(t, err)
	assert.Len(t, inputs, 1)
	rows, err := store.ListSchedules(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, rows[0].fireCount)
	require.NotNil(t, rows[0].lastFiredAt)
	assert.True(t, minute.Add(time.Second).Equal(*rows[0].lastFiredAt))
	assert.Empty(t, sessions.TakeWoken())
}

func TestDeliverCronEnqueueFailureLeavesScheduleUnfired(t *testing.T) {
	t.Parallel()
	store, sessions, root := newDeliveryStore(t)
	sched, err := store.AddSchedule(t.Context(), root, "* * * * *", nil, "tick", false, ScheduleMetadata{})
	require.NoError(t, err)
	require.NoError(t, sessions.UpdateSessionStatus(t.Context(), root, sessionstore.SessionStatusKilled))
	now := time.Now().UTC()
	applied, err := store.DeliverCron(t.Context(), sched, "tick", nil, now.Truncate(time.Minute), now)
	require.ErrorIs(t, err, sessionstore.ErrSessionNotAcceptingInput)
	assert.False(t, applied)
	rows, err := store.ListSchedules(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].lastFiredAt)
	assert.Zero(t, rows[0].fireCount)
	inputs, err := sessions.ListPending(t.Context(), root)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	assert.Empty(t, sessions.TakeWoken())
}

func TestDeliverSleepStaleCallDeletesRowOnly(t *testing.T) {
	t.Parallel()
	store, sessions, root := newDeliveryStore(t)
	_, err := sessions.Commit(t.Context(), sessionstore.Commit{SessionID: root, Messages: []*transcript.Message{
		{Role: "assistant", ToolCalls: []byte(`[{"ID":"sleep-call","Name":"sleep","Arguments":"e30="}]`)},
	}})
	require.NoError(t, err)
	now := time.Now().UTC()
	sched, err := store.AddSchedule(
		t.Context(),
		root,
		"",
		&now,
		"wake",
		false,
		ScheduleMetadata{ToolCallID: "sleep-call"},
	)
	require.NoError(t, err)
	due, err := store.ListDueSchedules(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, due, 1)
	_, err = sessions.Commit(t.Context(), sessionstore.Commit{SessionID: root, Messages: []*transcript.Message{
		{Role: "tool", ToolCallID: "sleep-call", ToolName: "sleep", Content: "interrupted"},
	}})
	require.NoError(t, err)
	sessions.TakeWoken()
	applied, err := store.DeliverSleep(t.Context(), sched)
	require.NoError(t, err)
	assert.False(t, applied)
	rows, err := store.ListSchedules(t.Context(), root)
	require.NoError(t, err)
	assert.Empty(t, rows)
	inputs, err := sessions.ListPending(t.Context(), root)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	assert.Empty(t, sessions.TakeWoken())
}

func TestDeliverOneShotEnqueueFailurePreservesRow(t *testing.T) {
	for _, sleep := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "sleep"}[sleep], func(t *testing.T) {
			t.Parallel()
			store, sessions, root := newDeliveryStore(t)
			meta := ScheduleMetadata{}
			if sleep {
				meta.ToolCallID = "sleep-call"
				_, err := sessions.Commit(
					t.Context(),
					sessionstore.Commit{SessionID: root, Messages: []*transcript.Message{
						{
							Role:      "assistant",
							ToolCalls: []byte(`[{"ID":"sleep-call","Name":"sleep","Arguments":"e30="}]`),
						},
					}},
				)
				require.NoError(t, err)
			}
			now := time.Now().UTC()
			sched, err := store.AddSchedule(t.Context(), root, "", &now, "wake", false, meta)
			require.NoError(t, err)
			require.NoError(t, sessions.UpdateSessionStatus(t.Context(), root, sessionstore.SessionStatusKilled))
			sessions.TakeWoken()
			var applied bool
			if sleep {
				applied, err = store.DeliverSleep(t.Context(), sched)
			} else {
				applied, err = store.DeliverOneShot(t.Context(), sched, "wake", nil)
			}
			require.ErrorIs(t, err, sessionstore.ErrSessionNotAcceptingInput)
			assert.False(t, applied)
			rows, err := store.ListSchedules(t.Context(), root)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, sched.id, rows[0].id)
			inputs, err := sessions.ListPending(t.Context(), root)
			require.NoError(t, err)
			assert.Empty(t, inputs)
			assert.Empty(t, sessions.TakeWoken())
		})
	}
}

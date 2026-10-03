package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestScheduledDeliveryToSubagentIsAcknowledgedWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		deliver func(*svc, int64) (bool, error)
	}{
		{
			name: "normal",
			deliver: func(mgr *svc, sessionID int64) (bool, error) {
				return enqueueScheduledInput(

					t.Context(), mgr.store,

					sessionID,
					"schedule:test:normal",
					"legacy task",
					false,
				)
			},
		},
		{
			name: "fresh",
			deliver: func(mgr *svc, sessionID int64) (bool, error) {
				return enqueueScheduledInput(

					t.Context(), mgr.store,

					sessionID,
					"schedule:test:fresh",
					"legacy fresh task",
					true,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, trivialRespond)
			defer h.shutdown()

			childID := createScheduleBoundarySubagent(t, h)
			before := h.parentMessages(childID)

			applied, err := tt.deliver(h.mgr, childID)
			require.NoError(t, err)
			assert.False(t, applied)
			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.parentMessages(childID))

			rec, err := h.sessStore.GetSession(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

func TestIntegration_LegacySubagentOneShotIsDiscardedWithoutRun(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	childID := createScheduleBoundarySubagent(t, h)
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedStore.AddSchedule(t.Context(), childID, "", &due, "legacy task", false)
	require.NoError(t, err)

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(t.Context())
	defer executor.Stop()

	require.Eventually(t, func() bool {
		entries, listErr := h.schedStore.ListSchedules(t.Context(), childID)
		return listErr == nil && len(entries) == 0
	}, 5*time.Second, 10*time.Millisecond)
	executor.Stop()

	assert.False(t, h.mgr.HasActiveLoop(childID))
	assert.Empty(t, h.parentMessages(childID))
	rec, err := h.sessStore.GetSession(t.Context(), childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
	assert.Zero(t, rec.Iteration)
}

func TestIntegration_LegacySubagentCronOccurrencesAreAcknowledgedWithoutRun(t *testing.T) {
	tests := []struct {
		name  string
		fresh bool
	}{
		{name: "normal"},
		{name: "fresh", fresh: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, trivialRespond)
			defer h.shutdown()

			childID := createScheduleBoundarySubagent(t, h)
			before := h.parentMessages(childID)
			collector := collectEvents(h.mgr.bus.SubscribeAll())
			defer collector.stop()

			entry, err := h.schedStore.AddSchedule(t.Context(), childID, "* * * * *", nil, "legacy task", tt.fresh)
			require.NoError(t, err)

			executor := schedule.NewExecutor(h.schedStore, h.mgr)
			executor.Start(t.Context())
			defer executor.Stop()

			require.Eventually(t, func() bool {
				schedules, listErr := h.schedStore.ListSchedules(t.Context(), childID)
				return listErr == nil && len(schedules) == 1 && schedules[0].ID() == entry.ID() &&
					schedules[0].LastFiredAt() != nil
			}, 5*time.Second, 10*time.Millisecond)
			executor.Stop()

			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.parentMessages(childID))
			assert.Empty(t, collector.snapshot(), "an acknowledged legacy occurrence is not published")
			rec, err := h.sessStore.GetSession(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

func createScheduleBoundarySubagent(t *testing.T, h *subagentHarness) int64 {
	t.Helper()

	parent, err := h.sessStore.CreateSession(t.Context(), h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(t.Context(), func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				t.Context(),
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		t.Context(), childID, sessionstore.SessionStatusCompleted,
	))

	return childID
}

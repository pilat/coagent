package schedule_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestExecutor_ReportAttributes(t *testing.T) {
	for _, reportTo := range []int64{0, 777} {
		t.Run(map[bool]string{false: "unreported", true: "reported"}[reportTo != 0], func(t *testing.T) {
			store, projects, sessions := newTestDB(t)
			project := testProject(t, projects, "/tmp/project")
			root, err := sessions.CreateSession(t.Context(), project, "model", "", nil)
			require.NoError(t, err)
			at := time.Now().Add(-time.Minute)
			created, err := store.AddSchedule(
				t.Context(),
				root.ID,
				"",
				&at,
				"scheduled input",
				false,
				schedule.ScheduleMetadata{ReportTo: reportTo},
			)
			require.NoError(t, err)
			executor := schedule.NewExecutor(store, &mockSender{})
			executor.Start(t.Context())
			t.Cleanup(executor.Stop)
			var inputs []*sessionstore.InboxInput
			require.Eventually(t, func() bool {
				inputs, err = sessions.ListPending(t.Context(), root.ID)
				return err == nil && len(inputs) == 1
			}, 5*time.Second, 10*time.Millisecond)
			assert.Equal(t, sessionstore.InputSourceSchedule, inputs[0].Source)
			if reportTo == 0 {
				assert.NotContains(t, inputs[0].Attributes, sessionstore.ObserverSessionIDAttribute)
				assert.NotContains(t, inputs[0].Attributes, "schedule_id")
			} else {
				assert.EqualValues(t, reportTo, inputs[0].Attributes[sessionstore.ObserverSessionIDAttribute])
				assert.EqualValues(t, created.ID(), inputs[0].Attributes["schedule_id"])
			}
		})
	}
}

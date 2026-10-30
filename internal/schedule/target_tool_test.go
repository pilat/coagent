package schedule_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/tool"
)

type targetedFixture struct {
	tool                tool.Tool
	svc                 schedule.Service
	store               schedule.Store
	self, work, foreign int64
	location            *time.Location
}

func TestTargetedScheduleCreate(t *testing.T) {
	for _, target := range []string{"self", "zero", "work"} {
		for _, once := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/once=%t", target, once), func(t *testing.T) {
				f := newTargetedFixture(t)
				p := map[string]any{"action": "create"}
				id := f.self
				if target == "zero" {
					p["session"] = "#0"
				}
				if target == "work" {
					p["session"], p["prompt"], p["fresh"], p["report"] = fmt.Sprint(
						f.work,
					), "scheduled work", true, true
					id = f.work
				}
				if once {
					p["at"] = time.Now().Add(48 * time.Hour).In(f.location).Format("2006-01-02 15:04")
				} else {
					p["cron"] = "0 9 * * *"
				}
				result, err := f.execute(t, p)
				require.NoError(t, err)
				assert.Contains(t, result, "Schedule created:")
				entries, err := f.svc.ListSchedules(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				assert.False(t, entries[0].Sleep())
				assert.Equal(t, once, entries[0].OneShotAt() != nil)
				if once {
					assert.NotEmpty(t, entries[0].InputMessage())
				}
				if target == "work" {
					assert.Equal(t, f.self, entries[0].ReportTo())
					assert.True(t, entries[0].Fresh())
				}
			})
		}
	}
}

func TestTargetedScheduleRefusals(t *testing.T) {
	f := newTargetedFixture(t)
	for _, tc := range []struct{ args, want string }{
		{`{"action":"create"}`, "exactly one"},
		{`{"action":"create","cron":"0 9 * * *","at":"tomorrow"}`, "exactly one"},
		{`{"action":"create","at":"tomorrow"}`, "invalid at"},
		{`{"action":"create","at":"2000-01-01 10:00"}`, "future"},
		{`{"action":"create","cron":"bad"}`, "invalid cron"},
		{`{"action":"create","cron":"CRON_TZ=UTC 0 9 * * *"}`, "invalid cron"},
		{`{"action":"create","cron":"CRON_TZ=UTC"}`, "invalid cron"},
		{`{"action":"create","cron":"0 9 * * *","report":true}`, "report applies to work-session schedules"},
		{fmt.Sprintf(`{"action":"create","session":%d,"cron":"0 9 * * *"}`, f.work), "requires a prompt"},
		{fmt.Sprintf(`{"action":"create","session":%d,"cron":"0 9 * * *","prompt":"work"}`, f.foreign), "not addressable"},
	} {
		_, err := f.tool.Execute(t.Context(), json.RawMessage(tc.args))
		require.ErrorContains(t, err, tc.want)
	}
	for _, id := range []int64{f.self, f.work, f.foreign} {
		entries, err := f.svc.ListSchedules(t.Context(), id)
		require.NoError(t, err)
		assert.Empty(t, entries)
	}
}

func TestTargetedScheduleListAndCancel(t *testing.T) {
	f := newTargetedFixture(t)
	own, err := f.svc.AddRecurring(t.Context(), f.self, "CRON_TZ=Europe/Berlin 0 9 * * *", "own", false)
	require.NoError(t, err)
	work, err := f.svc.AddOnce(t.Context(), f.work, time.Now().Add(time.Hour), "work", true, f.self)
	require.NoError(t, err)
	foreign, err := f.svc.AddRecurring(t.Context(), f.foreign, "0 9 * * *", "foreign", false)
	require.NoError(t, err)
	var sleeps []int64
	for _, id := range []int64{f.self, f.work} {
		created, err := f.svc.AddSleep(t.Context(), id, "pending-call", time.Now().Add(time.Hour), "sleep")
		require.NoError(t, err)
		sleeps = append(sleeps, created.ID)
	}
	list, err := f.execute(t, map[string]any{"action": "list"})
	require.NoError(t, err)
	assert.Contains(t, list, fmt.Sprintf("id=%d you · cron 0 9 * * * (Europe/Berlin)", own.ID))
	assert.Contains(t, list, fmt.Sprintf("id=%d #%d work · purpose · once ", work.ID, f.work))
	assert.Contains(t, list, " · fresh · report")
	assert.Contains(t, list, " · next ")
	for _, id := range append(sleeps, foreign.ID) {
		assert.NotContains(t, list, fmt.Sprintf("id=%d ", id))
		_, err := f.execute(t, map[string]any{"action": "cancel", "id": id})
		require.ErrorContains(t, err, fmt.Sprintf("schedule %d is not yours", id))
	}
	for _, id := range []int64{own.ID, work.ID} {
		result, err := f.execute(t, map[string]any{"action": "cancel", "id": id})
		require.NoError(t, err)
		assert.Contains(t, result, "cancelled")
	}
	entries, err := f.svc.ListSchedules(t.Context(), f.work)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].Sleep())
}

func newTargetedFixture(t *testing.T) targetedFixture {
	t.Helper()
	store, projects, sessions := newTestDB(t)
	project := testProject(t, projects, "/tmp/project")
	var ids []int64
	for range 3 {
		root, err := sessions.CreateSession(t.Context(), project, "model", "", nil)
		require.NoError(t, err)
		ids = append(ids, root.ID)
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	svc := schedule.NewService(store, sessions)
	resolve := func(_ context.Context, id int64) (string, error) {
		if id != ids[1] {
			return "", fmt.Errorf("session %d not addressable", id)
		}
		return fmt.Sprintf("#%d work · purpose", id), nil
	}
	list := func(context.Context) ([]schedule.Target, error) {
		return []schedule.Target{{ID: ids[1], Name: fmt.Sprintf("#%d work · purpose", ids[1])}}, nil
	}
	return targetedFixture{
		tool:     schedule.NewTargetedScheduleTool(ids[0], svc, loc, resolve, list),
		svc:      svc,
		store:    store,
		self:     ids[0],
		work:     ids[1],
		foreign:  ids[2],
		location: loc,
	}
}

func (f targetedFixture) execute(t *testing.T, p map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	result, err := f.tool.Execute(t.Context(), raw)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

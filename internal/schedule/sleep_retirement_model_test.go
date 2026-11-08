package schedule_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type (
	sleepRetirer   uint8
	retirementStep uint8
)

const (
	retiredByReader sleepRetirer = iota
	retiredByTimer
	retiredByStop
)

const (
	stepSettle retirementStep = iota
	stepProject
	stepProjectLosingRace
)

const (
	settledSleepCall = "settled-sleep"
	liveSleepCall    = "live-sleep"
)

// Reference model: a settled call's row leaves once, whoever retires it, and projecting it never fails.
type sleepRetirementModel struct {
	callPending, settledRow, liveRow bool
}

func (m *sleepRetirementModel) project(race *sleepRetirer) []string {
	visible := []string{}
	liveListed := m.liveRow
	switch {
	case m.settledRow && m.callPending:
		visible = append(visible, settledSleepCall)
	case m.settledRow:
		m.settledRow = false
		if race != nil && *race == retiredByStop {
			m.liveRow = false
		}
	}
	if liveListed {
		visible = append(visible, liveSleepCall)
	}
	return visible
}

func (m *sleepRetirementModel) rows() int {
	rows := 0
	for _, present := range []bool{m.settledRow, m.liveRow} {
		if present {
			rows++
		}
	}
	return rows
}

// Another owner retires the row after this caller listed it and before this caller deletes it.
type retiredFirstStore struct {
	schedule.Store
	retire func(context.Context, int64) error
}

func (s *retiredFirstStore) CallPending(ctx context.Context, sessionID int64, callID string) (bool, error) {
	pending, err := s.Store.CallPending(ctx, sessionID, callID)
	if err != nil || pending || s.retire == nil {
		return pending, err
	}
	retire := s.retire
	s.retire = nil
	return false, retire(ctx, sessionID)
}

func TestHarnessModel_StaleSleepRowRetiresOnceInAnyOrder(t *testing.T) {
	retirers := map[string]sleepRetirer{"reader": retiredByReader, "timer": retiredByTimer, "stop": retiredByStop}
	orders := [][]retirementStep{
		{stepSettle, stepProject, stepProjectLosingRace},
		{stepSettle, stepProjectLosingRace, stepProject},
		{stepProject, stepSettle, stepProjectLosingRace},
		{stepProject, stepProjectLosingRace, stepSettle},
		{stepProjectLosingRace, stepSettle, stepProject},
		{stepProjectLosingRace, stepProject, stepSettle},
	}
	store, projects, sessions := newTestDB(t)
	for name, retirer := range retirers {
		for i, order := range orders {
			t.Run(fmt.Sprintf("%s/%d", name, i), func(t *testing.T) {
				runRetirementTrace(t, store, projects, sessions, retirer, order)
			})
		}
	}
}

func runRetirementTrace(
	t *testing.T,
	store schedule.Store,
	projects, sessions *sessionstore.Store,
	retirer sleepRetirer,
	order []retirementStep,
) {
	t.Helper()
	ctx := t.Context()
	race := &retiredFirstStore{Store: store}
	svc := schedule.NewService(race, sessions)
	id := seedSleeps(t, projects, sessions, svc)
	model := &sleepRetirementModel{callPending: true, settledRow: true, liveRow: true}
	project := func(modelRace *sleepRetirer) {
		t.Helper()
		pending, err := svc.PendingSleeps(ctx, id)
		require.NoError(t, err)
		got := make([]string, 0, len(pending))
		for _, sleep := range pending {
			got = append(got, sleep.CallID)
		}
		assert.ElementsMatch(t, model.project(modelRace), got)
		rows, err := store.ListSchedules(ctx, id)
		require.NoError(t, err)
		assert.Len(t, rows, model.rows())
	}
	for _, step := range order {
		switch step {
		case stepSettle:
			model.callPending = false
			_, err := sessions.Commit(ctx, sessionstore.Commit{SessionID: id, ToolResults: []*transcript.Message{
				{Role: llmwire.RoleTool, ToolCallID: settledSleepCall, ToolName: "sleep", Content: "Sleep interrupted"},
			}})
			require.NoError(t, err)
		case stepProject:
			project(nil)
		case stepProjectLosingRace:
			race.retire = retirerFunc(store, retirer)
			project(&retirer)
			race.retire = nil
		}
	}
	project(nil)
}

func retirerFunc(store schedule.Store, retirer sleepRetirer) func(context.Context, int64) error {
	return func(ctx context.Context, id int64) error {
		if retirer == retiredByStop {
			_, err := store.RemoveSleepSchedules(ctx, id)
			return err
		}
		rows, err := store.ListSchedules(ctx, id)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.InputMessage() != settledSleepCall {
				continue
			}
			if retirer == retiredByReader {
				return store.RemoveSchedule(ctx, row.ID())
			}
			_, err := store.DeliverSleep(ctx, row)
			return err
		}
		return fmt.Errorf("settled sleep row missing for session %d", id)
	}
}

func seedSleeps(t *testing.T, projects, sessions *sessionstore.Store, svc schedule.Service) int64 {
	t.Helper()
	ctx := t.Context()
	root, err := sessions.CreateSession(ctx, testProject(t, projects, "/tmp/project"), "", "", nil)
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{SessionID: root.ID, Messages: []*transcript.Message{
		{
			Role: "assistant",
			ToolCalls: fmt.Appendf(
				nil,
				`[{"ID":%q,"Name":"sleep","Arguments":"e30="},{"ID":%q,"Name":"sleep","Arguments":"e30="}]`,
				settledSleepCall,
				liveSleepCall,
			),
		},
	}})
	require.NoError(t, err)
	for _, call := range []string{settledSleepCall, liveSleepCall} {
		_, err = svc.AddSleep(ctx, root.ID, call, time.Now().Add(time.Hour), call)
		require.NoError(t, err)
	}
	return root.ID
}

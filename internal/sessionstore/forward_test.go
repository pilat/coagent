package sessionstore

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardExactHumanInputAndReceipt(t *testing.T) {
	f := newHumanObserverFixture(t)
	require.NoError(
		t,
		f.store.SetAttributes(t.Context(), f.work, map[string]any{"manager_id": "manager", "title": "investigate"}),
	)
	input := forwardRunInput(t, f, "  **yes**\n\n> do it\r\n  ", true)
	result, err := forwardFixture(t, f, f.work)
	require.NoError(t, err)
	require.True(t, result.Applied)
	assert.Equal(t, input.RawContent, result.Input.RawContent)
	assert.NotContains(t, result.Input.Attributes, "from")
	assert.EqualValues(t, f.observer, result.Input.Attributes[ObserverSessionIDAttribute])
	assert.EqualValues(t, f.observer, result.Input.Attributes[ForwardedByAttribute])
	assert.EqualValues(t, input.ID, result.Input.Attributes[ForwardedInputAttribute])
	assert.True(t, IsHumanInput(result.Input.Source, result.Input.Attributes))
	assert.Equal(t, fmt.Sprintf("forward:%d", input.ID), result.Input.DeliveryKey)
	var content, key string
	var target int64
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), `SELECT content,source_key,json_extract(attributes,'$.forwarded_session_id')
		FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, f.observer).Scan(&content, &key, &target),
	)
	assert.Equal(t, fmt.Sprintf("→ #%d test · investigate (forwarded)", f.work), content)
	assert.Equal(t, fmt.Sprintf("forward:%d:%d", input.ID, f.work), key)
	assert.Equal(t, f.work, target)
	_, err = acceptInput(t.Context(), f.store, result.Input.ID, result.Input.RawContent)
	require.NoError(t, err)
	f.final(t, "answer", false)
	events := f.events(t)
	require.Len(t, events, 2)
	assert.Contains(t, events[1].RawContent, fmt.Sprintf("→ test (%d) you forwarded: %q", f.work, input.RawContent))
}

func TestForwardRollbackAndIdempotence(t *testing.T) {
	f := newHumanObserverFixture(t)
	forwardRunInput(t, f, "yes", true)
	cause := errors.New("rollback")
	err := f.store.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := ForwardTx(t.Context(), tx, f.observer, f.work, nil)
		require.NoError(t, err)
		return cause
	})
	require.ErrorIs(t, err, cause)
	assertForwardCounts(t, f, f.work, 0, 0)
	for i := range 2 {
		result, err := forwardFixture(t, f, f.work)
		require.NoError(t, err)
		assert.Equal(t, i == 0, result.Applied)
	}
	assertForwardCounts(t, f, f.work, 1, 1)
	other := chatTestRoot(t, f.store, f.project, "manager")
	result, err := forwardFixture(t, f, other.ID)
	require.NoError(t, err)
	assert.True(t, result.Applied)
	assertForwardCounts(t, f, other.ID, 1, 2)
}

func TestForwardSlashBarriers(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			f := newHumanObserverFixture(t)
			if !owned {
				require.NoError(t, f.store.SetAttributes(t.Context(), f.work, nil))
			}
			input := forwardRunInput(t, f, " \n/budget 5", true)
			_, err := forwardFixture(t, f, f.work)
			require.ErrorIs(t, err, ErrForwardedCommand)
			_, err = f.store.Enqueue(t.Context(), Input{
				SessionID: f.work, Source: InputSourceUser,
				Content: input.RawContent, Attributes: map[string]any{ForwardedByAttribute: f.observer},
			})
			require.ErrorIs(t, err, ErrForwardedCommand)
			assertForwardCounts(t, f, f.work, 0, 0)
		})
	}
}

func TestForwardNeverLiftsBudget(t *testing.T) {
	f := newHumanObserverFixture(t)
	input := forwardRunInput(t, f, "continue", true)
	armed, err := f.store.Arm(t.Context(), targetedBudgetMutation(t, f.store, f.work, f.work, "arm"))
	require.NoError(t, err)
	_, _, err = f.store.FireBudget(t.Context(), f.work, armed.Generation, "duration", 0, "checkpoint")
	require.NoError(t, err)
	_, err = forwardFixture(t, f, f.work)
	require.ErrorIs(t, err, ErrBudgetParked)
	record, err := f.store.Get(t.Context(), f.work)
	require.NoError(t, err)
	assert.Equal(t, "fired", string(record.State))
	assertForwardCounts(t, f, f.work, 1, 0)
	_, err = f.store.Enqueue(t.Context(), Input{SessionID: f.work, Source: InputSourceUser, Content: input.RawContent})
	require.NoError(t, err)
	record, err = f.store.Get(t.Context(), f.work)
	require.NoError(t, err)
	assert.Equal(t, "released", string(record.State))
}

func TestActiveHumanInputBelongsToCurrentRun(t *testing.T) {
	f := newHumanObserverFixture(t)
	input := forwardRunInput(t, f, "current human", true)
	_, err := f.store.Commit(t.Context(), observerFinal(f.observer, "answer", false))
	require.NoError(t, err)
	current, err := f.store.ActiveHumanInput(t.Context(), f.observer)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, input.ID, current.ID)
	event, err := f.store.Enqueue(
		t.Context(),
		Input{SessionID: f.observer, Source: InputSourceAgent, Content: "mid-run event"},
	)
	require.NoError(t, err)
	_, err = acceptInput(t.Context(), f.store, event.Input.ID, event.Input.RawContent)
	require.NoError(t, err)
	assistant, err := f.store.Enqueue(t.Context(), Input{
		SessionID: f.observer, Source: InputSourceUser,
		Content: "assistant's words", Attributes: map[string]any{"from": "assistant"},
	})
	require.NoError(t, err)
	_, err = acceptInput(t.Context(), f.store, assistant.Input.ID, assistant.Input.RawContent)
	require.NoError(t, err)
	_, err = f.db.ExecContext(
		t.Context(),
		`UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE session_id = ?`,
		f.observer,
	)
	require.NoError(t, err)
	for range 2 {
		_, err = forwardFixture(t, f, f.work)
		require.NoError(t, err)
	}
	other := chatTestRoot(t, f.store, f.project, "manager")
	_, err = forwardFixture(t, f, other.ID)
	require.NoError(t, err)
	for _, withEvent := range []bool{true, false} {
		step := Commit{SessionID: f.observer, BeginRun: true}
		if withEvent {
			event, err := f.store.Enqueue(
				t.Context(),
				Input{SessionID: f.observer, Source: InputSourceAgent, Content: "later event"},
			)
			require.NoError(t, err)
			step.Accept = []Accept{
				{
					InputID:    event.Input.ID,
					State:      InputStateAccepted,
					Content:    event.Input.RawContent,
					LinkRef:    -1,
					ModelBound: true,
				},
			}
		}
		_, err := f.store.Commit(t.Context(), step)
		require.NoError(t, err)
		f.store = NewStore(f.db)
		current, err = f.store.ActiveHumanInput(t.Context(), f.observer)
		require.NoError(t, err)
		assert.Nil(t, current)
		_, err = forwardFixture(t, f, other.ID)
		require.ErrorIs(t, err, ErrForwardInput)
		assertForwardCounts(t, f, f.work, 1, 2)
	}
	newest := forwardRunInput(t, f, "new human", false)
	current, err = f.store.ActiveHumanInput(t.Context(), f.observer)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, newest.ID, current.ID)
	result, err := forwardFixture(t, f, f.work)
	require.NoError(t, err)
	require.True(t, result.Applied)
	assert.Equal(t, newest.RawContent, result.Input.RawContent)
	assert.EqualValues(t, newest.ID, result.Input.Attributes[ForwardedInputAttribute])
}

func TestRunAcceptanceRollback(t *testing.T) {
	f := newHumanObserverFixture(t)
	old := forwardRunInput(t, f, "old", true)
	event, err := f.store.Enqueue(t.Context(), Input{SessionID: f.observer, Source: InputSourceAgent, Content: "event"})
	require.NoError(t, err)
	_, err = f.store.Commit(t.Context(), Commit{
		SessionID: f.observer, BeginRun: true,
		Accept:  []Accept{{InputID: event.Input.ID, State: InputStateAccepted, Content: "event", LinkRef: -1}},
		Outputs: []Output{{Type: OutputMessage, Phase: "final", MessageRef: 999}},
	})
	require.Error(t, err)
	current, err := f.store.ActiveHumanInput(t.Context(), f.observer)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, old.ID, current.ID)
	pending, err := f.store.ListPending(t.Context(), f.observer)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, event.Input.ID, pending[0].ID)
}

func forwardRunInput(t *testing.T, f observerFixture, text string, begin bool) *InboxInput {
	t.Helper()
	in, err := f.store.Enqueue(t.Context(), Input{SessionID: f.observer, Source: InputSourceUser, Content: text})
	require.NoError(t, err)
	_, err = f.store.Commit(t.Context(), Commit{
		SessionID: f.observer,
		BeginRun:  begin,
		Accept: []Accept{
			{InputID: in.Input.ID, State: InputStateAccepted, Content: text, LinkRef: -1, ModelBound: true},
		},
	})
	require.NoError(t, err)
	current, err := f.store.ActiveHumanInput(t.Context(), f.observer)
	require.NoError(t, err)
	require.NotNil(t, current)
	return current
}

func forwardFixture(t *testing.T, f observerFixture, target int64) (*Enqueued, error) {
	t.Helper()
	var result *Enqueued
	err := f.store.WithTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		result, err = ForwardTx(t.Context(), tx, f.observer, target, nil)
		return err
	})
	return result, err
}

func assertForwardCounts(t *testing.T, f observerFixture, target int64, inputs, outputs int) {
	t.Helper()
	var gotInputs, gotOutputs int
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, target).
			Scan(&gotInputs),
	)
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, f.observer).
			Scan(&gotOutputs),
	)
	assert.Equal(t, inputs, gotInputs)
	assert.Equal(t, outputs, gotOutputs)
}

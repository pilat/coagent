package sessionstore

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

type observerFixture struct {
	store                   *Store
	db                      *sql.DB
	work, observer, project int64
}

func TestObserverNoReferences(t *testing.T) {
	f := newObserverFixture(t)
	f.store.TakeWoken()
	result := f.final(t, "answer", false)
	require.Len(t, result.MessageIDs, 1)
	require.Len(t, result.Outputs, 1)
	assert.Empty(t, f.events(t))
	assert.Equal(t, []int64{f.work}, f.store.TakeWoken())
	assert.Equal(t, int64(1), f.count(t, "messages"))
	assert.Equal(t, int64(1), f.count(t, "session_outbox"))
}

func TestObserverAnswerWindow(t *testing.T) {
	f := newObserverFixture(t)
	_, err := appendMessage(
		t.Context(),
		f.store,
		f.work,
		&transcript.Message{Role: "assistant", Content: "before window"},
	)
	require.NoError(t, err)
	f.ref(t, true)
	f.ref(t, true)
	f.final(t, " substantial yield ", true)
	assert.Empty(t, f.events(t))
	_, err = appendMessages(t.Context(), f.store, f.work, []*transcript.Message{
		{Role: "assistant", Content: "substantial yield"},
		{Role: "assistant", Content: "rejected", RejectedReason: RejectedReasonOutputLength, FinishType: "length"},
		{Role: "user", Content: "host text"},
	})
	require.NoError(t, err)
	compacted, err := appendMessage(
		t.Context(),
		f.store,
		f.work,
		&transcript.Message{Role: "assistant", Content: "compacted"},
	)
	require.NoError(t, err)
	_, err = f.db.ExecContext(
		t.Context(),
		`UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE id = ?`,
		compacted,
	)
	require.NoError(t, err)
	direct, err := enqueueInput(t.Context(), f.store, f.work, InputSourceUser, "direct user follow-up")
	require.NoError(t, err)
	_, err = acceptInput(t.Context(), f.store, direct.ID, "direct user follow-up")
	require.NoError(t, err)
	f.store.TakeWoken()
	result := f.final(t, "closing final", false)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(
		t,
		fmt.Sprintf("← test (%d) answered: %q", f.work, "substantial yield\n\n---\n\nclosing final"),
		events[0].RawContent,
	)
	assert.Equal(t, fmt.Sprintf("observer:%d", result.Outputs[0].OutputID), events[0].DeliveryKey)
	assert.Equal(t, InputSourceAgent, events[0].Source)
	assert.EqualValues(t, f.work, events[0].Attributes[ObservedSessionIDAttribute])
	assert.Equal(t, "answered", events[0].Attributes[ObservedKindAttribute])
	assert.EqualValues(t, result.MessageIDs[0], events[0].Attributes[ObservedMessageIDAttribute])
	assert.ElementsMatch(t, []int64{f.work, f.observer}, f.store.TakeWoken())
	f.final(t, "no new instruction", false)
	assert.Len(t, f.events(t), 1)
	f.ref(t, true)
	f.final(t, "next answer", false)
	events = f.events(t)
	require.Len(t, events, 2)
	assert.Equal(t, fmt.Sprintf("← test (%d) answered: %q", f.work, "next answer"), events[1].RawContent)
}

func TestObserverAnchorStates(t *testing.T) {
	for _, state := range []InputState{InputStatePending, InputStateAccepted, InputStateHandled, InputStateRejected, InputStateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, true)
			f.final(t, "first answer", false)
			events := f.events(t)
			require.Len(t, events, 1)
			if state == InputStateAccepted {
				_, err := acceptInput(t.Context(), f.store, events[0].ID, events[0].RawContent)
				require.NoError(t, err)
			} else if state != InputStatePending {
				_, err := f.store.Commit(t.Context(), Commit{SessionID: f.observer, Accept: []Accept{{
					InputID: events[0].ID, State: state, Reason: "test", LinkRef: -1,
				}}})
				require.NoError(t, err)
			}
			f.final(t, "second answer", false)
			want := 1
			if state == InputStateCancelled {
				want = 2
			}
			assert.Len(t, f.events(t), want)
		})
	}
}

func TestObserverFailureWindows(t *testing.T) {
	for _, unfired := range []bool{false, true} {
		t.Run(fmt.Sprint(unfired), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, true)
			pending := f.ref(t, false)
			other, err := f.store.CreateSession(t.Context(), f.project, "model", "", nil)
			require.NoError(t, err)
			_, err = f.store.Enqueue(t.Context(), Input{
				SessionID: f.work, Source: InputSourceUser, Content: "other observer",
				Attributes: map[string]any{ObserverSessionIDAttribute: other.ID},
			})
			require.NoError(t, err)
			f.store.TakeWoken()
			step := Commit{SessionID: f.work, State: StatePatch{Status: new(SessionStatusError)}, Outputs: []Output{
				{
					Type:          OutputMessagePersistent,
					Content:       "error notice",
					Key:           "failure",
					ReleasesInput: true,
					MessageRef:    -1,
				},
			}}
			if unfired {
				step.Unfired = Parts{State: step.State, Outputs: step.Outputs}
				step.State = StatePatch{}
				step.Outputs = nil
			}
			result, err := f.store.Commit(t.Context(), step)
			require.NoError(t, err)
			events := f.events(t)
			require.Len(t, events, 1)
			assert.Equal(t, fmt.Sprintf("← test (%d) failed: error notice", f.work), events[0].RawContent)
			assert.NotContains(t, events[0].Attributes, ObservedMessageIDAttribute)
			assert.Equal(t, fmt.Sprintf("observer:%d", result.Outputs[0].OutputID), events[0].DeliveryKey)
			assert.ElementsMatch(t, []int64{f.work, f.observer, other.ID}, f.store.TakeWoken())
			otherEvents, err := f.store.ListPending(t.Context(), other.ID)
			require.NoError(t, err)
			require.Len(t, otherEvents, 1)
			_, err = acceptInput(t.Context(), f.store, pending, "pending instruction")
			require.NoError(t, err)
			f.final(t, "recovered", false)
			events = f.events(t)
			require.Len(t, events, 2)
			assert.Contains(t, events[1].RawContent, `answered: "recovered"`)
		})
	}
}

func TestObserverStartFailure(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, false)
	f.store.TakeWoken()
	for attempt := range 2 {
		reported, err := f.store.RecordSessionStartFailure(t.Context(), f.work, "cannot start")
		require.NoError(t, err)
		assert.Equal(t, attempt == 0, reported)
	}
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(t, fmt.Sprintf("← test (%d) failed: cannot start", f.work), events[0].RawContent)
	assert.NotContains(t, events[0].Attributes, ObservedMessageIDAttribute)
	assert.Equal(t, []int64{f.observer}, f.store.TakeWoken())
}

func TestObserverEnqueueRollback(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprint(start), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, !start)
			_, err := f.db.ExecContext(
				t.Context(),
				fmt.Sprintf(`CREATE TRIGGER fail_observer BEFORE INSERT ON session_inbox
WHEN NEW.session_id = %d BEGIN SELECT RAISE(ABORT, 'observer insert failed'); END`, f.observer),
			)
			require.NoError(t, err)
			before := f.count(t, "messages")
			f.store.TakeWoken()
			if start {
				_, err = f.store.RecordSessionStartFailure(t.Context(), f.work, "cannot start")
			} else {
				_, err = f.store.Commit(t.Context(), observerFinal(f.work, "answer", false))
			}
			require.ErrorContains(t, err, "observer insert failed")
			assert.Equal(t, before, f.count(t, "messages"))
			assert.Zero(t, f.count(t, "session_outbox"))
			assert.Empty(t, f.events(t))
			assert.Empty(t, f.store.TakeWoken())
			record, err := f.store.GetSession(t.Context(), f.work)
			require.NoError(t, err)
			assert.Equal(t, SessionStatusActive, record.Status)
		})
	}
}

func TestObserverKilledIsSkipped(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	_, err := f.db.ExecContext(
		t.Context(),
		`UPDATE sessions SET killed_at = CURRENT_TIMESTAMP, status = 'killed' WHERE id = ?`,
		f.observer,
	)
	require.NoError(t, err)
	f.final(t, "skipped", false)
	assert.Empty(t, f.events(t))
	_, err = f.db.ExecContext(
		t.Context(),
		`UPDATE sessions SET killed_at = NULL, status = 'active' WHERE id = ?`,
		f.observer,
	)
	require.NoError(t, err)
	f.final(t, "later", false)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].RawContent, `answered: "skipped\n\n---\n\nlater"`)
}

func TestObserverReplay(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	step := observerFinal(f.work, "answer", false)
	step.Outputs[0].Key = "replayed-final"
	first, err := f.store.Commit(t.Context(), step)
	require.NoError(t, err)
	events := f.events(t)
	require.Len(t, events, 1)
	_, err = f.store.Commit(t.Context(), Commit{SessionID: f.observer, Accept: []Accept{{
		InputID: events[0].ID, State: InputStateCancelled, Reason: "stop", LinkRef: -1,
	}}})
	require.NoError(t, err)
	replay, err := f.store.Commit(t.Context(), step)
	require.NoError(t, err)
	assert.Equal(t, first.Outputs[0].OutputID, replay.Outputs[0].OutputID)
	assert.Len(t, f.events(t), 1)
	assert.Equal(t, int64(1), f.count(t, "session_outbox"))
}

func TestObserverDistinctObservers(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	f.final(t, "yield", true)
	second, err := f.store.CreateSession(t.Context(), f.project, "model", "", nil)
	require.NoError(t, err)
	other := f
	other.observer = second.ID
	other.ref(t, true)
	f.final(t, "final", false)
	a, b := f.events(t), other.events(t)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	assert.Contains(t, a[0].RawContent, `"yield\n\n---\n\nfinal"`)
	assert.Contains(t, b[0].RawContent, `"final"`)
	assert.Equal(t, a[0].DeliveryKey, b[0].DeliveryKey)
}

func TestObserverProtocolAcrossRestart(t *testing.T) {
	f := newObserverFixture(t)
	open := false
	var parts, expected []string
	for index, action := range []string{"ref", "yield", "restart", "failure", "restart", "answer", "answer", "ref", "restart", "answer"} {
		t.Run(fmt.Sprintf("%d_%s", index, action), func(t *testing.T) {
			switch action {
			case "ref":
				f.ref(t, true)
				open = true
			case "yield":
				f.final(t, "yield", true)
				parts = append(parts, "yield")
			case "restart":
				f.store = NewStore(f.db)
			case "failure":
				_, err := f.store.RecordSessionStartFailure(t.Context(), f.work, "unavailable")
				require.NoError(t, err)
				if open {
					expected = append(expected, fmt.Sprintf("← test (%d) failed: unavailable", f.work))
				}
			case "answer":
				f.final(t, "answer", false)
				if open {
					parts = append(parts, "answer")
					expected = append(
						expected,
						fmt.Sprintf("← test (%d) answered: %q", f.work, strings.Join(parts, "\n\n---\n\n")),
					)
					open, parts = false, nil
				}
			}
			var actual []string
			for _, event := range f.events(t) {
				actual = append(actual, event.RawContent)
			}
			assert.Equal(t, expected, actual)
		})
	}
}

func TestObserverOwnerlessAndChildDoNotNotify(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprint(child), func(t *testing.T) {
			f := newObserverFixture(t)
			_, err := f.db.ExecContext(t.Context(), `UPDATE sessions SET attributes = '{}' WHERE id = ?`, f.work)
			require.NoError(t, err)
			if child {
				_, err = f.db.ExecContext(
					t.Context(),
					`UPDATE sessions SET parent_id = ?, root_id = ? WHERE id = ?`,
					f.observer,
					f.observer,
					f.work,
				)
				require.NoError(t, err)
			}
			f.ref(t, true)
			f.final(t, "answer", false)
			_, err = f.store.RecordSessionStartFailure(t.Context(), f.work, "failure")
			require.NoError(t, err)
			assert.Empty(t, f.events(t))
		})
	}
}

func TestObserverClipsWholeEvent(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	f.final(t, "HEAD"+strings.Repeat("я", 25000)+"TAIL", false)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(t, 20000, utf8.RuneCountInString(events[0].RawContent))
	assert.True(t, strings.HasPrefix(events[0].RawContent, "← test"))
	assert.Contains(t, events[0].RawContent, "HEAD")
	assert.Contains(t, events[0].RawContent, "\n[…]\n")
	assert.True(t, strings.HasSuffix(events[0].RawContent, `TAIL"`))
}

func newObserverFixture(t *testing.T) observerFixture {
	t.Helper()
	store, db, project := newTestStore(t)
	work, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "manager"})
	require.NoError(t, err)
	observer, err := store.CreateSession(t.Context(), project, "model", "", nil)
	require.NoError(t, err)
	return observerFixture{store: store, db: db, work: work.ID, observer: observer.ID, project: project}
}

func observerFinal(work int64, text string, yield bool) Commit {
	return Commit{
		SessionID: work,
		Messages:  []*transcript.Message{{Role: "assistant", Content: text, FinishType: "stop"}},
		State:     StatePatch{Status: new(SessionStatusCompleted)},
		Outputs: []Output{
			{
				Type:          OutputMessagePersistent,
				Content:       text,
				Phase:         "final",
				MessageRef:    0,
				ReleasesInput: true,
				Yield:         yield,
			},
		},
	}
}

func (f observerFixture) ref(t *testing.T, accepted bool) int64 {
	t.Helper()
	in, err := f.store.Enqueue(t.Context(), Input{
		SessionID: f.work, Source: InputSourceUser, Content: "instruction",
		Attributes: map[string]any{ObserverSessionIDAttribute: f.observer},
	})
	require.NoError(t, err)
	if accepted {
		_, err = acceptInput(t.Context(), f.store, in.Input.ID, "instruction")
		require.NoError(t, err)
	}
	return in.Input.ID
}

func (f observerFixture) final(t *testing.T, text string, yield bool) *CommitResult {
	t.Helper()
	result, err := f.store.Commit(t.Context(), observerFinal(f.work, text, yield))
	require.NoError(t, err)
	return result
}

func (f observerFixture) events(t *testing.T) []*InboxInput {
	t.Helper()
	rows, err := f.db.QueryContext(
		t.Context(),
		`SELECT `+inboxColumns+` FROM session_inbox WHERE session_id = ? ORDER BY id`,
		f.observer,
	)
	require.NoError(t, err)
	defer rows.Close()
	var events []*InboxInput
	for rows.Next() {
		in, err := scanInboxInput(rows)
		require.NoError(t, err)
		events = append(events, in)
	}
	require.NoError(t, rows.Err())
	return events
}

func (f observerFixture) count(t *testing.T, table string) int64 {
	t.Helper()
	query := map[string]string{
		"messages":       `SELECT COUNT(*) FROM messages WHERE session_id = ?`,
		"session_outbox": `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`,
	}[table]
	var count int64
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), query, f.work).Scan(&count),
	)
	return count
}

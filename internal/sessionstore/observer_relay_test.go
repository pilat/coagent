package sessionstore

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

type relayTestRow struct {
	id, origin, output int64
	content, key       string
}

func TestObserverRelayFinalsAndCandidate(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	yield := f.final(t, "  yield\nanswer  ", true)
	assert.Empty(t, relayRows(t, f))
	candidate, err := f.store.Commit(t.Context(), Commit{
		SessionID: f.work,
		Messages: []*transcript.Message{
			{Role: "assistant", Content: "confirmed candidate", FinishType: "stop"},
			{Role: "user", Content: "second look"},
		},
		State: StatePatch{Candidate: &CandidateChange{NextRef: 0}, MarkCompletionNudge: true},
	})
	require.NoError(t, err)
	id := candidate.MessageIDs[0]
	step := observerFinal(f.work, "post-nudge acknowledgement", false)
	step.Outputs[0].Content = "confirmed candidate"
	step.State.Candidate = &CandidateChange{Expected: id, NextRef: -1}
	step.State.ConfirmedAnswerID = &id
	final, err := f.store.Commit(t.Context(), step)
	require.NoError(t, err)
	require.Len(t, final.Outputs, 1)
	rows := relayRows(t, f)
	require.Len(t, rows, 2)
	assert.Equal(t, yield.Outputs[0].OutputID, rows[0].output)
	assert.Equal(t, final.Outputs[0].OutputID, rows[1].output)
	for _, row := range rows {
		var original string
		require.NoError(
			t,
			f.db.QueryRowContext(t.Context(), `SELECT content FROM session_outbox WHERE id = ?`, row.output).
				Scan(&original),
		)
		assert.Equal(t, original, row.content)
		assert.Equal(t, fmt.Sprintf("relay:%d", row.output), row.key)
		assert.Equal(t, f.work, row.origin)
	}
	assert.Equal(t, "  yield\nanswer  ", rows[0].content)
	assert.Equal(t, "confirmed candidate", rows[1].content)
	require.NoError(t, f.store.SetSessionTitle(t.Context(), "manager", f.work, "renamed"))
	outputs, err := f.store.ChatOutputs(t.Context(), "manager", "chat", f.observer, 10)
	require.NoError(t, err)
	require.Len(t, outputs, 2)
	assert.Equal(t, f.work, outputs[1].Origin)
	assert.Equal(t, "test · renamed", outputs[1].OriginName)
	assert.Equal(t, rows[1].content, outputs[1].Content)
	require.NoError(t, f.store.MarkChatSeen(t.Context(), "manager", "chat", f.observer, outputs[1].ID))
	last, found, err := f.store.LastSeenChatOutput(t.Context(), "manager", "chat", f.observer)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, outputs[1], last)
	seen, err := f.store.ChatSeen(t.Context(), "manager", "chat", f.work)
	require.NoError(t, err)
	assert.Zero(t, seen)
}

func TestObserverRelayFocusPredicate(t *testing.T) {
	for _, focus := range []string{"unbound", "assistant", "work", "other"} {
		t.Run(focus, func(t *testing.T) {
			f := newObserverFixture(t)
			if focus != "unbound" {
				require.NoError(t, f.store.BindChat(t.Context(), "manager", "chat"))
			}
			if focus == "work" {
				require.NoError(t, f.store.SetChatFocus(t.Context(), "manager", "chat", f.work))
			}
			if focus == "other" {
				require.NoError(t, f.store.SetChatFocus(t.Context(), "manager", "chat", f.observer))
			}
			f.ref(t, true)
			f.final(t, "answer", false)
			assert.Len(t, f.events(t), 1)
			want := 1
			if focus == "work" {
				want = 0
			}
			assert.Len(t, relayRows(t, f), want)
		})
	}
}

func TestObserverRelayFailureNotices(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprint(start), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, !start)
			if start {
				_, err := f.store.RecordSessionStartFailure(t.Context(), f.work, "start\nfailure")
				require.NoError(t, err)
			} else {
				status := SessionStatusError
				_, err := f.store.Commit(t.Context(), Commit{
					SessionID: f.work,
					State:     StatePatch{Status: &status},
					Outputs: []Output{
						{
							Type:          OutputMessage,
							Content:       "run\nfailure",
							Key:           "run:error",
							MessageRef:    -1,
							ReleasesInput: true,
						},
					},
				})
				require.NoError(t, err)
			}
			rows := relayRows(t, f)
			require.Len(t, rows, 1)
			var original string
			require.NoError(
				t,
				f.db.QueryRowContext(t.Context(), `SELECT content FROM session_outbox WHERE id = ?`, rows[0].output).
					Scan(&original),
			)
			assert.Equal(t, original, rows[0].content)
			assert.Len(t, f.events(t), 1)
		})
	}
}

func TestObserverRelayCancelledEventIsIdempotent(t *testing.T) {
	f := newObserverFixture(t)
	f.ref(t, true)
	f.final(t, "first", false)
	events := f.events(t)
	require.Len(t, events, 1)
	_, err := f.store.Commit(t.Context(), Commit{
		SessionID: f.observer,
		Accept:    []Accept{{InputID: events[0].ID, State: InputStateCancelled, Reason: "stop", LinkRef: -1}},
	})
	require.NoError(t, err)
	f.store = NewStore(f.db)
	f.final(t, "second", false)
	rows := relayRows(t, f)
	require.Len(t, rows, 2)
	assert.Equal(t, "first", rows[0].content)
	assert.Equal(t, "second", rows[1].content)
	assert.Len(t, f.events(t), 2)
	f.final(t, "unobserved", false)
	assert.Len(t, relayRows(t, f), 2)
}

func TestObserverRelayClosedObserverIsSkipped(t *testing.T) {
	for _, status := range []SessionStatus{SessionStatusKilled, SessionStatusStopping, SessionStatusTerminating} {
		t.Run(string(status), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, true)
			require.NoError(t, f.store.UpdateSessionStatus(t.Context(), f.observer, status))
			tx, err := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			require.ErrorIs(t, outputSessionWritable(t.Context(), tx, f.observer), ErrOutputNotWritable)
			require.NoError(t, tx.Rollback())
			result := f.final(t, "answer", false)
			assert.Len(t, result.Outputs, 1)
			assert.Empty(t, f.events(t))
			assert.Empty(t, relayRows(t, f))
		})
	}
}

func TestObserverRelayReadError(t *testing.T) {
	f := newObserverFixture(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	err = relayObservedTx(t.Context(), tx, f.work, 0, 0, "answer",
		observerWindow{observer: f.observer}, time.Now(), CommitLoop)
	require.ErrorIs(t, err, sql.ErrTxDone)
	assert.Empty(t, relayRows(t, f))
}

func TestObserverRelayInsertFailureRollsBackWork(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprint(start), func(t *testing.T) {
			f := newObserverFixture(t)
			f.ref(t, !start)
			before := f.count(t, "messages")
			f.store.TakeWoken()
			_, err := f.db.ExecContext(
				t.Context(),
				fmt.Sprintf(`CREATE TRIGGER fail_relay BEFORE INSERT ON session_outbox
				WHEN NEW.session_id = %d BEGIN SELECT RAISE(ABORT, 'relay insert failed'); END`, f.observer),
			)
			require.NoError(t, err)
			if start {
				_, err = f.store.RecordSessionStartFailure(t.Context(), f.work, "cannot start")
			} else {
				_, err = f.store.Commit(t.Context(), observerFinal(f.work, "answer", false))
			}
			require.Error(t, err)
			assert.Equal(t, before, f.count(t, "messages"))
			assert.Zero(t, f.count(t, "session_outbox"))
			assert.Empty(t, relayRows(t, f))
			assert.Empty(t, f.events(t))
			assert.Empty(t, f.store.TakeWoken())
			record, err := f.store.GetSession(t.Context(), f.work)
			require.NoError(t, err)
			assert.Equal(t, SessionStatusActive, record.Status)
		})
	}
}

func relayRows(t *testing.T, f observerFixture) []relayTestRow {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), `SELECT id, content, source_key,
		json_extract(attributes,'$.relayed_session_id'), json_extract(attributes,'$.relayed_output_id')
		FROM session_outbox WHERE session_id = ? AND source_key LIKE 'relay:%' ORDER BY id`, f.observer)
	require.NoError(t, err)
	defer rows.Close()
	var result []relayTestRow
	for rows.Next() {
		var row relayTestRow
		require.NoError(t, rows.Scan(&row.id, &row.content, &row.key, &row.origin, &row.output))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}

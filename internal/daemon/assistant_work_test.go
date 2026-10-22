package daemon

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAssistantWorkReachesEveryOwnedRoot(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	a := assistantFor(h, root.ID)
	touched := workWithAnswer(t, h, root.ID, true, "real user", "answer\n"+strings.Repeat("я", 600))
	userMade := workWithAnswer(t, h, root.ID, false, "собери отчёт по продажам", "отчёт готов")
	foreign, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs("another-manager"))
	require.NoError(t, err)
	killed := catalogTestWork(t, h, root.ID, h.projectID, true)
	_, err = h.db.ExecContext(
		h.ctx,
		`UPDATE sessions SET killed_at=CURRENT_TIMESTAMP,status='killed' WHERE id=?`,
		killed,
	)
	require.NoError(t, err)

	recent, total, err := a.Recent(h.ctx, 10)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	assert.Equal(t, 1, total)
	assert.Equal(t, touched, recent[0].ID)
	assert.Equal(t, "done", recent[0].Status)
	assert.True(t, recent[0].Pending)
	assert.Equal(t, "real user", recent[0].About, "without a stored about the first request stands in")
	_, err = h.db.ExecContext(h.ctx, `UPDATE sessions SET context_baseline_prompt_tokens = 40000 WHERE id = ?`, touched)
	require.NoError(t, err)
	recent, _, err = a.Recent(h.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 40000, recent[0].ContextTokens, "the desk shows what continuing the session costs")

	listed, total, err := a.List(h.ctx, 0, 20, "")
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.ElementsMatch(t, []int64{touched, userMade}, []int64{listed[0].ID, listed[1].ID})

	hits, err := a.Search(h.ctx, "отчёт*")
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, hit := range hits {
		assert.Equal(t, userMade, hit.Session, "a root the user created is searchable")
	}

	card, err := a.Target(h.ctx, userMade)
	require.NoError(t, err)
	assert.Equal(t, userMade, card.ID)
	require.NoError(t, a.Send(h.ctx, userMade, "продолжи отчёт"))
	var sent int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM assistant_sends WHERE session_id = ?`,
		userMade).Scan(&sent))
	assert.Equal(t, 1, sent, "a root the user created is sendable")
	for _, id := range []int64{foreign.ID, killed, root.ID} {
		_, err := a.Target(h.ctx, id)
		require.ErrorContains(t, err, "not a work session you can address", id)
	}

	text, err := a.Read(h.ctx, touched, 60)
	require.NoError(t, err)
	assert.Contains(t, text, "user: real user")
	assert.Contains(t, text, "answer\n"+strings.Repeat("я", 600), "a message is shown whole")
	assert.NotContains(t, text, "host nudge")
	_, err = a.Read(h.ctx, root.ID, 1)
	require.Error(t, err)
}

func TestAssistantReadIncludesCompactedFinals(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	a := assistantFor(h, root.ID)
	work := workWithAnswer(t, h, root.ID, true, "old request", "old final")
	_, err = h.db.ExecContext(h.ctx, `UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE session_id = ?`, work)
	require.NoError(t, err)
	answerAgain(t, h, work, "new request", "new final")
	// Compaction numbers only the kept tail; the compacted rows keep a NULL position.
	_, err = h.db.ExecContext(h.ctx, `UPDATE messages SET position = id - (SELECT MIN(id) FROM messages m
		WHERE m.session_id = ? AND m.compacted_at IS NULL) + 1 WHERE session_id = ? AND compacted_at IS NULL`, work, work)
	require.NoError(t, err)
	text, err := a.Read(h.ctx, work, 10)
	require.NoError(t, err)
	assert.Equal(t, "user: old request\n\nassistant: old final\n\nuser: new request\n\nassistant: new final", text)
	text, err = a.Read(h.ctx, work, 2)
	require.NoError(t, err)
	assert.Equal(t, "user: new request\n\nassistant: new final", text, "the newest entries are the last ones")
}

func TestAssistantRecentOrdersByActivity(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	a := assistantFor(h, root.ID)
	var ids []int64
	for index := range 12 {
		id := catalogTestWork(t, h, root.ID, h.projectID, true)
		ids = append(ids, id)
		_, err = h.db.ExecContext(h.ctx, `UPDATE sessions SET updated_at=? WHERE id=?`,
			time.Now().UTC().Add(time.Duration(index)*time.Minute), id)
		require.NoError(t, err)
	}
	_, err = h.db.ExecContext(h.ctx, `UPDATE assistant_sends SET observed_at=CURRENT_TIMESTAMP WHERE session_id!=?`,
		ids[11])
	require.NoError(t, err)
	recent, total, err := a.Recent(h.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 12, total)
	require.Len(t, recent, 10)
	assert.Equal(t, ids[11], recent[0].ID)
	assert.True(t, recent[0].Pending)
	assert.False(t, recent[1].Pending)
	assert.Equal(t, ids[2], recent[9].ID)
	page, _, err := a.List(h.ctx, 10, 20, "")
	require.NoError(t, err)
	assert.Equal(t, []int64{ids[1], ids[0]}, []int64{page[0].ID, page[1].ID})
}

func TestAssistantOpenAboutFailureRollsBackFirstSend(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	_, err = h.db.ExecContext(h.ctx, `CREATE TRIGGER reject_about BEFORE INSERT ON assistant_work
		BEGIN SELECT RAISE(ABORT,'about unavailable'); END`)
	require.NoError(t, err)
	a := assistantFor(h, root.ID)
	_, err = a.Open(h.ctx, "about-failure", "must not run", "purpose")
	require.ErrorContains(t, err, "about unavailable")
	var sends, inputs int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM assistant_sends`).Scan(&sends))
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT COUNT(*) FROM session_inbox WHERE raw_content='must not run'`).Scan(&inputs))
	assert.Zero(t, sends)
	assert.Zero(t, inputs)
}

// workWithAnswer gives a completed root one accepted request, a host nudge and a final answer.
func workWithAnswer(t *testing.T, h *harness, root int64, touched bool, ask, answer string) int64 {
	t.Helper()
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	in, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: work.ID, Source: sessionstore.InputSourceUser,
		Content: ask,
	})
	require.NoError(t, err)
	if touched {
		require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(h.ctx, `INSERT INTO assistant_sends
				(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
				in.Input.ID, root, work.ID, time.Now().UTC())

			return err
		}))
	}
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: ask, LinkRef: -1},
		},
		Messages: []*transcript.Message{
			{Role: "user", Content: "host nudge"},
			{Role: "assistant", Content: answer, FinishType: "stop"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, work.ID, sessionstore.SessionStatusCompleted))

	return work.ID
}

func answerAgain(t *testing.T, h *harness, work int64, ask, answer string) {
	t.Helper()
	in, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: work, Source: sessionstore.InputSourceUser,
		Content: ask,
	})
	require.NoError(t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work,
		Accept: []sessionstore.Accept{
			{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: ask, LinkRef: -1},
		},
		Messages: []*transcript.Message{{Role: "assistant", Content: answer, FinishType: "stop"}},
	})
	require.NoError(t, err)
}

func catalogTestWork(t *testing.T, h *harness, root, project int64, touched bool) int64 {
	t.Helper()
	work, err := h.store.CreateSession(h.ctx, project, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	if !touched {
		return work.ID
	}
	in, err := h.store.Enqueue(h.ctx,
		sessionstore.Input{SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "work"})
	require.NoError(t, err)
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, `INSERT INTO assistant_sends
			(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
			in.Input.ID, root, work.ID, time.Now().UTC())

		return err
	}))

	return work.ID
}

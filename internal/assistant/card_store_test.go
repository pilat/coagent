package assistant

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAssistantCardTreeCost(t *testing.T) {
	t.Parallel()
	db, sessions, store, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	child, err := sessions.CreateSubagentSession(t.Context(), 1, work, work, "general", "model", "high")
	require.NoError(t, err)
	for id, cost := range map[int64]float64{work: 10, child: 0.8} {
		_, err := sessions.Commit(t.Context(), sessionstore.Commit{
			SessionID: id, Messages: []*transcript.Message{{Role: "assistant", Content: "answer", CostUSD: cost}},
		})
		require.NoError(t, err)
	}
	now := time.Now().UTC()
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 10, 2)`, work, now)
	require.NoError(t, err)
	scope := Scope{Root: root, Owner: testOwner}
	card, err := store.Target(t.Context(), scope, work)
	require.NoError(t, err)
	require.NotNil(t, card.TreeCostUSD)
	assert.InDelta(t, 10.8, *card.TreeCostUSD, 1e-9)
	require.NotNil(t, card.Budget)
	assert.True(t, now.Equal(card.Budget.ArmedAt))
	assert.Equal(t, "model", card.Model)
	assert.Equal(t, "medium", card.Effort)
	assert.Contains(t, cardLines(card, now, nil), "$1.20 left")
	_, err = db.ExecContext(t.Context(), `UPDATE session_budgets SET cost_limit_usd = NULL,
		duration_seconds = 120 WHERE root_session_id = ?`, work)
	require.NoError(t, err)
	card, err = store.Target(t.Context(), scope, work)
	require.NoError(t, err)
	assert.Nil(t, card.TreeCostUSD)
	other := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	card, err = store.Target(t.Context(), scope, other)
	require.NoError(t, err)
	assert.Nil(t, card.TreeCostUSD)
	assert.Nil(t, card.Budget)
}

func TestAssistantCardScheduled(t *testing.T) {
	t.Parallel()
	db, sessions, store, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	for _, metadata := range []string{`{"tool_call_id":"sleep-call"}`, `{}`} {
		_, err := db.ExecContext(
			t.Context(),
			`INSERT INTO schedules(session_id,one_shot_at,metadata,input_message) VALUES (?,?,?,'')`,
			work,
			time.Now().Add(time.Hour),
			metadata,
		)
		require.NoError(t, err)
		card, err := store.Target(t.Context(), Scope{Root: root, Owner: testOwner}, work)
		require.NoError(t, err)
		assert.Equal(t, metadata == `{}`, card.Scheduled)
	}
}

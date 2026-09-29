package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// The model records obligations created by admission, then resolves each from
// the durable inbox/output pair after the stop transition. It does not model
// the implementation's drain phases.
type parkInputModel struct {
	id      int64
	compact bool
	result  string
}

type budgetParkModel struct {
	inputs []parkInputModel
	parked bool
}

func (m *budgetParkModel) accept(id int64, compact bool) {
	m.inputs = append(m.inputs, parkInputModel{id: id, compact: compact})
}

func (m *budgetParkModel) settle(failed bool) {
	if failed {
		return
	}
	for i := range m.inputs {
		if m.inputs[i].compact {
			m.inputs[i].result = string(sessionstore.InputStateHandled)
		} else {
			m.inputs[i].result = string(sessionstore.InputStateCancelled)
		}
	}
	m.parked = true
}

func TestBudgetParkModel_CompactObligationsSurviveFailedSettlement(t *testing.T) {
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	model := budgetParkModel{}
	accept := func(content string, compact bool) int64 {
		input, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, content)
		require.NoError(t, err)
		model.accept(input.ID, compact)
		return input.ID
	}

	accept("ordinary before the fence", false)
	compactID := accept("/compact preserve this command", true)
	record := fireBudgetForParkTest(t, h, sessionID)

	// A durable output failure leaves the admitted obligations unresolved.
	_, err := h.db.ExecContext(h.ctx, fmt.Sprintf(`CREATE TRIGGER fail_model_park_output
		BEFORE INSERT ON session_outbox
		WHEN NEW.source_key = 'input:%d:compact:parked'
		BEGIN SELECT RAISE(FAIL, 'model trace settlement failure'); END`, compactID))
	require.NoError(t, err)
	model.settle(true)
	h.mgr.parkBudgetTree(h.ctx, record)
	assertBudgetParkModel(t, h, sessionID, &model)

	// Admission after the stopping fence is rejected and creates no obligation.
	err = h.mgr.SendToSession(h.ctx, sessionID, "late input")
	require.Error(t, err)
	assertBudgetParkModel(t, h, sessionID, &model)

	_, err = h.db.ExecContext(h.ctx, `DROP TRIGGER fail_model_park_output`)
	require.NoError(t, err)
	retry, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	model.settle(false)
	h.mgr.parkBudgetTree(h.ctx, retry)
	assertBudgetParkModel(t, h, sessionID, &model)
	require.True(t, model.parked)
}

func assertBudgetParkModel(t *testing.T, h *subagentHarness, sessionID int64, model *budgetParkModel) {
	t.Helper()
	for _, expected := range model.inputs {
		var state string
		require.NoError(t, h.db.QueryRowContext(h.ctx,
			`SELECT state FROM session_inbox WHERE id = ?`, expected.id).Scan(&state))
		var outputs int
		sourceKey := fmt.Sprintf("input:%d:compact:parked", expected.id)
		require.NoError(t, h.db.QueryRowContext(h.ctx,
			`SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key = ?`,
			sessionID, sourceKey).Scan(&outputs))

		if expected.result == "" {
			require.Equal(t, string(sessionstore.InputStatePending), state,
				"unresolved accepted input must remain available for retry")
			require.Zero(t, outputs, "unresolved input cannot already have a terminal output")
			continue
		}

		require.Equal(t, expected.result, state)
		if expected.compact {
			require.Equal(t, 1, outputs, "accepted /compact must have exactly one durable result")
		} else {
			require.Zero(t, outputs, "cancelled ordinary input must not get a compact result")
		}
	}

	budget, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	if model.parked {
		require.Equal(t, "parked", budget.ParkPhase)
	} else {
		require.Equal(t, "draining", budget.ParkPhase)
	}
}

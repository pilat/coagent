package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestBudgetParkSettlesCompactBehindPendingUserInput(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	normalInput, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, "ordinary input")
	require.NoError(t, err)
	compactInput, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, "/compact focus")
	require.NoError(t, err)
	secondCompact, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, "/compact")
	require.NoError(t, err)
	record := fireBudgetForParkTest(t, h, sessionID)
	callsBeforePark := modelCalls.Load()

	h.mgr.parkBudgetTree(h.ctx, record)

	require.Equal(t, callsBeforePark, modelCalls.Load(), "parking must settle /compact without another model call")
	assertParkedCompactState(t, h, sessionID, compactInput.ID)
	assertParkedCompactState(t, h, sessionID, secondCompact.ID)
	var normalState string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, normalInput.ID).Scan(&normalState))
	require.Equal(t, string(sessionstore.InputStateCancelled), normalState)
}

func TestBudgetParkRetriesCompactOutputAfterInsertFailure(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	compactInput, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, "/compact")
	require.NoError(t, err)
	record := fireBudgetForParkTest(t, h, sessionID)
	callsBeforePark := modelCalls.Load()
	_, err = h.db.ExecContext(h.ctx, fmt.Sprintf(`CREATE TRIGGER fail_parked_compact_output
		BEFORE INSERT ON session_outbox
		WHEN NEW.source_key = 'input:%d:compact:parked'
		BEGIN SELECT RAISE(FAIL, 'injected parked compact output failure'); END`, compactInput.ID))
	require.NoError(t, err)

	h.mgr.parkBudgetTree(h.ctx, record)
	budget, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, "draining", budget.ParkPhase)
	sessionRecord, err := h.sessStore.GetSession(h.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopping, sessionRecord.Status)
	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, compactInput.ID).Scan(&state))
	require.Equal(t, string(sessionstore.InputStatePending), state)

	_, err = h.db.ExecContext(h.ctx, `DROP TRIGGER fail_parked_compact_output`)
	require.NoError(t, err)
	retry, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	h.mgr.parkBudgetTree(h.ctx, retry)

	require.Equal(t, callsBeforePark, modelCalls.Load(), "retry must settle /compact without a model call")
	assertParkedCompactState(t, h, sessionID, compactInput.ID)
}

func TestBudgetParkRecoversFailedLiveCompactTerminalWrite(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	fireBudgetForParkTest(t, h, sessionID)
	callsBeforeCompact := modelCalls.Load()
	_, err := h.db.ExecContext(h.ctx, `CREATE TRIGGER fail_live_compact_parked_output
		BEFORE INSERT ON session_outbox
		WHEN NEW.source_key LIKE 'input:%:compact:parked'
		BEGIN SELECT RAISE(FAIL, 'injected parked compact output failure'); END`)
	require.NoError(t, err)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/compact"))
	h.waitUntil("fired compact command survives failed live and park writes", func() bool {
		budget, budgetErr := h.sessStore.GetBudget(h.ctx, sessionID)
		if budgetErr != nil || budget.ParkPhase != "draining" {
			return false
		}
		sessionRecord, sessionErr := h.sessStore.GetSession(h.ctx, sessionID)
		return sessionErr == nil && sessionRecord.Status == sessionstore.SessionStatusStopping &&
			!h.mgr.HasActiveLoop(sessionID)
	})
	input, err := h.mgr.inboxStore.PeekPending(h.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, "/compact", input.RawContent)
	require.Equal(t, sessionstore.InputStatePending, input.State)
	require.Equal(t, callsBeforeCompact, modelCalls.Load(), "the failed terminal write must not call the model")

	_, err = h.db.ExecContext(h.ctx, `DROP TRIGGER fail_live_compact_parked_output`)
	require.NoError(t, err)
	retry, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	h.mgr.parkBudgetTree(h.ctx, retry)
	assertParkedCompactState(t, h, sessionID, input.ID)
	require.Equal(t, callsBeforeCompact, modelCalls.Load())
}

func TestBudgetParkRestartsFailedCompactSettlement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "park.db")
	respond := func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "ready"}
	}
	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	defer first.shutdown()

	sessionID := newBudgetParkCompactRoot(t, first)
	input, err := first.mgr.inboxStore.EnqueueInput(first.ctx, sessionID,
		sessionstore.InputSourceUser, "/compact")
	require.NoError(t, err)
	record := fireBudgetForParkTest(t, first, sessionID)
	_, err = first.db.ExecContext(first.ctx, `CREATE TRIGGER fail_restarted_compact_output
		BEFORE INSERT ON session_outbox
		WHEN NEW.source_key LIKE 'input:%:compact:parked'
		BEGIN SELECT RAISE(FAIL, 'injected parked compact output failure'); END`)
	require.NoError(t, err)

	first.mgr.parkBudgetTree(first.ctx, record)
	_, err = first.db.ExecContext(first.ctx, `DROP TRIGGER fail_restarted_compact_output`)
	require.NoError(t, err)

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	defer second.shutdown()
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("restart finishes the pending budget park", func() bool {
		budget, err := second.sessStore.GetBudget(second.ctx, sessionID)
		return err == nil && budget.ParkPhase == "parked"
	})
	var inputState string
	require.NoError(t, second.db.QueryRowContext(second.ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&inputState))
	require.Equal(t, string(sessionstore.InputStateHandled), inputState)
	assertParkedCompactState(t, second, sessionID, input.ID)
	assertParkedCompactDeliveredOnce(t, second, input.ID)
}

func TestBudgetParkDoesNotTreatCompactifyAsCompact(t *testing.T) {
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	nearMiss, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID, sessionstore.InputSourceUser, "/compactify")
	require.NoError(t, err)
	record := fireBudgetForParkTest(t, h, sessionID)
	h.mgr.parkBudgetTree(h.ctx, record)

	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, nearMiss.ID).Scan(&state))
	require.Equal(t, string(sessionstore.InputStateCancelled), state)
	var outputs int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = ?`, sessionID,
		fmt.Sprintf("input:%d:compact:parked", nearMiss.ID)).Scan(&outputs))
	require.Zero(t, outputs)
}

func TestExplicitStopStillCancelsPendingCompact(t *testing.T) {
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID := newBudgetParkCompactRoot(t, h)
	input, err := h.mgr.inboxStore.EnqueueInput(h.ctx, sessionID,
		sessionstore.InputSourceUser, "/compact")
	require.NoError(t, err)
	require.NoError(t, h.mgr.Stop(h.ctx, sessionID, 0))

	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&state))
	require.Equal(t, string(sessionstore.InputStateCancelled), state)
}

func newBudgetParkCompactRoot(t *testing.T, h *subagentHarness) int64 {
	t.Helper()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "prepare root", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)
	return sessionID
}

func fireBudgetForParkTest(t *testing.T, h *subagentHarness, sessionID int64) *sessionstore.BudgetRecord {
	t.Helper()
	_, err := h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)
	record, _, err := h.sessStore.FireBudget(h.ctx, sessionID, 1, "cost", 1, "budget fired")
	require.NoError(t, err)
	require.Equal(t, budgetParkRequested, record.ParkPhase)
	require.NotEmpty(t, record.ParkOwner)
	return record
}

func assertParkedCompactState(t *testing.T, h *subagentHarness, sessionID, inputID int64) {
	t.Helper()
	budget, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, "parked", budget.ParkPhase)
	sessionRecord, err := h.sessStore.GetSession(h.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopped, sessionRecord.Status)
	var inputState, outputType, sourceKey, content string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT i.state, o.type, o.source_key, o.content
		FROM session_inbox i JOIN session_outbox o ON o.session_id = i.session_id
		WHERE i.id = ? AND o.source_key = ?`, inputID, fmt.Sprintf("input:%d:compact:parked", inputID)).
		Scan(&inputState, &outputType, &sourceKey, &content))
	require.Equal(t, string(sessionstore.InputStateHandled), inputState)
	require.Equal(t, string(sessionstore.OutputMessagePersistent), outputType)
	require.Equal(t, fmt.Sprintf("input:%d:compact:parked", inputID), sourceKey)
	require.Equal(t, "⏸ Budget checkpoint reached — the session is parked. Send a message to resume.", content)
	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key = ?`,
		sessionID, sourceKey).Scan(&count))
	require.Equal(t, 1, count)
}

func assertParkedCompactDeliveredOnce(t *testing.T, h *subagentHarness, inputID int64) {
	t.Helper()
	controller := newChainController(t, h)
	sourceKey := fmt.Sprintf("input:%d:compact:parked", inputID)
	parkedClaims := 0
	for range 32 {
		claim, err := controller.ClaimOutput(t.Context())
		if errors.Is(err, controllerapi.ErrNoOutput) {
			break
		}
		require.NoError(t, err)
		if claim.SourceKey == sourceKey {
			parkedClaims++
			require.Equal(t, controllerapi.OutputMessagePersistent, claim.Type)
			require.Equal(t, session.ParkedCompactionNotice, claim.Content)
		}

		ack := controllerapi.OutputAckData{ID: claim.ID, AttemptID: claim.AttemptID}
		if claim.Type == controllerapi.OutputMessagePersistent || claim.Type == controllerapi.OutputMessageReplaceable {
			ack.MessageIDs = []string{fmt.Sprintf("delivered-%d", claim.ID)}
		}
		require.NoError(t, controller.AckOutput(t.Context(), ack))
	}

	require.Equal(t, 1, parkedClaims, "manager delivery must observe the parked command once")
	_, err := controller.ClaimOutput(t.Context())
	require.ErrorIs(t, err, controllerapi.ErrNoOutput)
}

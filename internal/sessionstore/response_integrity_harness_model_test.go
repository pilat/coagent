package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

const (
	integrityLength integrityProtocolCommand = iota
	integrityStop
	integrityRestart
)

type integrityProtocolCommand byte

type integrityProtocolModel struct {
	attempts   int
	rejected   int
	recoveries int
	iteration  int
	usageOut   int
	status     SessionStatus
}

type integrityProtocolProduction struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	store  Store
	rootID int64
}

func newIntegrityProtocolModel() *integrityProtocolModel {
	return &integrityProtocolModel{status: SessionStatusActive}
}

func (m *integrityProtocolModel) apply(command integrityProtocolCommand) {
	switch command {
	case integrityLength:
		m.attempts++
		m.rejected++
		m.iteration++
		m.usageOut += 10
		if m.recoveries == 0 {
			m.recoveries = 1
		} else {
			m.status = SessionStatusError
		}
	case integrityStop:
		if m.status == SessionStatusError {
			return
		}
		m.attempts++
		m.iteration++
		m.usageOut += 3
		m.status = SessionStatusCompleted
	case integrityRestart:
	}
}

func newIntegrityProtocolProduction(t *testing.T) *integrityProtocolProduction {
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(t.Context(), projectID, "model", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	return &integrityProtocolProduction{t: t, ctx: t.Context(), db: db, store: store, rootID: root.ID}
}

func (p *integrityProtocolProduction) apply(command integrityProtocolCommand) {
	switch command {
	case integrityLength:
		rejection := rejectedLength(p.rootID, p.currentIteration()+1, 0)
		rejection.Message.Usage = []byte(`{"promptTokens":5,"completionTokens":10}`)
		result, err := p.store.CommitRejectedResponse(p.ctx, rejection)
		require.NoError(p.t, err)
		require.Contains(p.t, []RejectedResponseOutcome{
			RejectedResponseRecoveryQueued, RejectedResponseRetryExhausted,
		}, result.Outcome)
	case integrityStop:
		if p.currentStatus() == SessionStatusError {
			return
		}
		usage := []byte(`{"promptTokens":2,"completionTokens":3}`)
		_, err := p.store.InsertMessage(p.ctx, p.rootID, &transcript.Message{
			Role: llmwire.RoleAssistant, Content: "done", FinishType: llmwire.FinishStop, Usage: usage,
		})
		require.NoError(p.t, err)
		require.NoError(p.t, p.store.UpdateSessionIteration(
			p.ctx, p.rootID, p.currentIteration()+1, SessionStatusCompleted,
		))
	case integrityRestart:
		p.store = NewStore(p.db)
	}
}

func (p *integrityProtocolProduction) currentIteration() int {
	var iteration int
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT iteration FROM sessions WHERE id = ?`, p.rootID).Scan(&iteration))

	return iteration
}

func (p *integrityProtocolProduction) currentStatus() SessionStatus {
	var status SessionStatus
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT status FROM sessions WHERE id = ?`, p.rootID).Scan(&status))

	return status
}

func (p *integrityProtocolProduction) assertMatches(model *integrityProtocolModel, step int) {
	var attempts, rejected, recoveries, usageOut int
	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT
		COUNT(*) FILTER (WHERE role = 'assistant' AND finish_type IS NOT NULL),
		COUNT(*) FILTER (WHERE rejected_reason IS NOT NULL),
		COUNT(*) FILTER (WHERE retry_of_message_id IS NOT NULL),
		COALESCE(SUM(json_extract(usage, '$.completionTokens')), 0)
		FROM messages WHERE session_id = ?`, p.rootID).
		Scan(&attempts, &rejected, &recoveries, &usageOut))

	assert.Equal(p.t, model.attempts, attempts, "step %d: attempts", step)
	assert.Equal(p.t, model.rejected, rejected, "step %d: rejected", step)
	assert.Equal(p.t, model.recoveries, recoveries, "step %d: recoveries", step)
	assert.Equal(p.t, model.iteration, p.currentIteration(), "step %d: iteration", step)
	assert.Equal(p.t, model.usageOut, usageOut, "step %d: usage", step)
	assert.Equal(p.t, model.status, p.currentStatus(), "step %d: status", step)

	if recoveries > 0 {
		var rejectedID, retryOf int64
		require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT id FROM messages
			WHERE session_id = ? AND rejected_reason = 'output_length' ORDER BY id LIMIT 1`, p.rootID).
			Scan(&rejectedID))
		require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT retry_of_message_id FROM messages
			WHERE session_id = ? AND retry_of_message_id IS NOT NULL`, p.rootID).Scan(&retryOf))
		assert.Equal(p.t, rejectedID, retryOf, "step %d: retry identity", step)
	}
}

func runIntegrityProtocol(t *testing.T, commands []integrityProtocolCommand) {
	t.Helper()
	model := newIntegrityProtocolModel()
	production := newIntegrityProtocolProduction(t)
	production.assertMatches(model, -1)

	for step, command := range commands {
		model.apply(command)
		production.apply(command)
		production.assertMatches(model, step)
	}
}

func TestHarnessModel_ResponseIntegrityLengthRestartStop(t *testing.T) {
	t.Parallel()

	runIntegrityProtocol(
		t,
		[]integrityProtocolCommand{integrityLength, integrityRestart, integrityStop, integrityRestart},
	)
}

func TestHarnessModel_ResponseIntegrityRepeatedLength(t *testing.T) {
	t.Parallel()

	runIntegrityProtocol(
		t,
		[]integrityProtocolCommand{integrityLength, integrityRestart, integrityLength, integrityRestart},
	)
}

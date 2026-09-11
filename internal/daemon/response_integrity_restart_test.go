package daemon

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestResponseIntegrity_RestartAfterFirstLengthPerformsOnlyOwedRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "response-restart.db")
	rootID := interruptFirstRecovery(t, dbPath)
	completeRecoveryAfterRestart(t, dbPath, rootID)
}

func interruptFirstRecovery(t *testing.T, dbPath string) int64 {
	t.Helper()
	secondCall := make(chan struct{})
	release := make(chan struct{})
	var calls int
	first := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return &llmwire.Response{Text: "discarded before restart", FinishType: llmwire.FinishLength}
		}
		close(secondCall)
		<-release
		return &llmwire.Response{Text: "must be canceled", FinishType: llmwire.FinishStop}
	}, nil)

	rootID, err := first.mgr.Send(first.ctx, first.projectID, "restart the recovery", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, secondCall, "recovery call before restart")
	var recoveryRows int
	require.NoError(t, first.db.QueryRowContext(first.ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND retry_of_message_id IS NOT NULL`, rootID).Scan(&recoveryRows))
	require.Equal(t, 1, recoveryRows)
	first.shutdown()
	close(release)

	return rootID
}

func completeRecoveryAfterRestart(t *testing.T, dbPath string, rootID int64) {
	t.Helper()
	var resumedCalls int
	second := newSubagentHarnessOnDB(t, dbPath, func(_ string, messages []llmwire.Message) *llmwire.Response {
		resumedCalls++
		visible := scenarioTranscriptText(messages)
		require.Contains(t, visible, sessionstore.OutputLengthRecoveryPrompt)
		require.NotContains(t, visible, "discarded before restart")
		return &llmwire.Response{Text: "recovered after restart", FinishType: llmwire.FinishStop}
	}, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	second.mgr.sweep(second.ctx)
	waitForVisibleMessage(t, collector, rootID, "recovered after restart")
	drainScenarioClaims(t, "unused-response-restart.json", newChainController(t, second))
	waitForIdleAfterMessage(t, collector, rootID, "recovered after restart")
	assert.Equal(t, 1, resumedCalls)
	second.shutdown()
	collector.stop()

	var unexpectedCalls atomic.Int64
	third := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		unexpectedCalls.Add(1)
		return &llmwire.Response{Text: "unexpected rerun", FinishType: llmwire.FinishStop}
	}, nil)
	defer third.shutdown()
	third.mgr.sweep(third.ctx)
	assert.Never(t, func() bool { return unexpectedCalls.Load() != 0 }, 300*time.Millisecond, 10*time.Millisecond)

	var rejectedRows, acceptedRows int
	require.NoError(t, third.db.QueryRowContext(third.ctx, `SELECT
		COUNT(*) FILTER (WHERE rejected_reason = 'output_length'),
		COUNT(*) FILTER (WHERE role = 'assistant' AND rejected_reason IS NULL AND finish_type = 'stop')
		FROM messages WHERE session_id = ?`, rootID).Scan(&rejectedRows, &acceptedRows))
	assert.Equal(t, 1, rejectedRows)
	assert.Equal(t, 1, acceptedRows)
}

func TestResponseIntegrity_RestartAfterTerminalFailureDoesNotRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "response-terminal-restart.db")
	first := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "discarded terminal", FinishType: llmwire.FinishUnknown}
	}, nil)
	rootID, err := first.mgr.Send(first.ctx, first.projectID, "fail terminally", "fake-model", nil)
	require.NoError(t, err)
	first.mgr.waitIdle(rootID)
	record, err := first.sessStore.GetSession(first.ctx, rootID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusError, record.Status)
	first.shutdown()

	var unexpectedCalls atomic.Int64
	second := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		unexpectedCalls.Add(1)
		return &llmwire.Response{Text: "unexpected rerun", FinishType: llmwire.FinishStop}
	}, nil)
	defer second.shutdown()
	second.mgr.sweep(second.ctx)
	assert.Never(t, func() bool { return unexpectedCalls.Load() != 0 }, 300*time.Millisecond, 10*time.Millisecond)
}

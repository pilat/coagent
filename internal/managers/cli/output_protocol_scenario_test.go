package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Two roots enqueue before CLI has subscribed, alongside a foreign root. The
// worker must hold the global CLI head while no terminal can acknowledge it,
// then preserve that order without leaking the foreign manager's output.
func TestHarnessScenario_CLIWorkerPreservesGlobalHeadAndForeignOwner(t *testing.T) {
	h := newDelayedCLIHarness(t)
	firstID := h.produceBeforeManager(t)
	h.waitForBacklog(t)
	secondID := h.produceBeforeManager(t)
	require.Eventually(t, func() bool {
		return h.outputStates(t, controllerapi.BuiltinCLIManagerID, 4) != nil
	}, 10*time.Second, 10*time.Millisecond, "both CLI roots must commit before manager startup")
	foreignID := h.produceForeignBeforeManager(t)
	require.Eventually(t, func() bool {
		return h.outputStates(t, "telegram-main", 2) != nil
	}, 10*time.Second, 10*time.Millisecond, "foreign root must carry its own output owner")

	terminal, _ := h.startManagerAndDial(t)
	require.Eventually(t, func() bool {
		states := h.outputStates(t, controllerapi.BuiltinCLIManagerID, 4)
		return len(states) == 4 && states[0] == sessionstore.OutputStateRetryWait &&
			states[1] == sessionstore.OutputStatePending && states[2] == sessionstore.OutputStatePending &&
			states[3] == sessionstore.OutputStatePending
	}, 10*time.Second, 10*time.Millisecond, "a failed global head must hide every later CLI row")

	opened := openChat(t, terminal)
	assert.Contains(t, []int64{firstID, secondID}, opened.SessionID)
	outputs := collectDurableCLIOutputs(t, terminal, 6)
	assert.Equal(t, []Event{
		{SessionID: firstID, Type: "session_opened"},
		{SessionID: firstID, Type: "message", Message: "delayed cli answer"},
		{SessionID: firstID, Type: "state_changed", Status: "idle"},
		{SessionID: secondID, Type: "session_opened"},
		{SessionID: secondID, Type: "message", Message: "delayed cli answer"},
		{SessionID: secondID, Type: "state_changed", Status: "idle"},
	}, normalizeCLIEvents(outputs))
	assert.Equal(t, []sessionstore.OutputState{
		sessionstore.OutputStatePending, sessionstore.OutputStatePending,
	}, h.outputStates(t, "telegram-main", 2), "the CLI worker must not claim foreign output")
	assert.NotEqual(t, firstID, foreignID)
}

// Generic lifecycle commands enter the inbox but their user-visible effects
// still leave through the output worker, never through direct controller pushes.
func TestHarnessScenario_CLIGenericLifecycleCommandsRenderFromOutbox(t *testing.T) {
	h := newDelayedCLIHarness(t)
	sessionID := h.produceBeforeManager(t)
	h.waitForBacklog(t)
	terminal, manager := h.startManagerAndDial(t)
	require.Equal(t, sessionID, openChat(t, terminal).SessionID)
	require.Equal(t, "delayed cli answer", waitForDelayedCLIMessage(t, terminal, sessionID).Message)

	cleared := sendDurableCLICommand(t, terminal, sessionID, "/clear")
	require.Equal(t, sessionID, cleared.SessionID, "the command is accepted by the old root before replacement")
	replacement := waitForCLIType(t, terminal, "session_replaced")
	assert.Equal(t, sessionID, replacement.OldSessionID)
	require.NotEqual(t, sessionID, replacement.SessionID)
	activeID, activeGeneration := manager.lifecycle()
	t.Logf(
		"replacement event generation=%d manager session=%d generation=%d",
		replacement.Generation,
		activeID,
		activeGeneration,
	)
	require.Equal(t, replacement.SessionID, activeID, "replacement delivery must update the input projection")

	killed := sendDurableCLICommand(t, terminal, replacement.SessionID, "/kill")
	require.Equal(t, replacement.SessionID, killed.SessionID)
	closed := waitForCLIType(t, terminal, "session_closed")
	assert.Equal(t, replacement.SessionID, closed.SessionID)
}

func (h *delayedCLIHarness) produceForeignBeforeManager(t *testing.T) int64 {
	t.Helper()
	sessionID, err := h.foreignController.CreateSession(t.Context(), controllerapi.SessionCreateData{
		WorkDir: filepath.Join(h.workDir, "foreign"), Prompt: "finish for telegram", Model: "fake-model",
	})
	require.NoError(t, err)

	return sessionID
}

func (h *delayedCLIHarness) outputStates(
	t *testing.T,
	managerID string,
	want int,
) []sessionstore.OutputState {
	t.Helper()
	rows, err := h.db.QueryContext(t.Context(), `SELECT state FROM session_outbox
		WHERE json_extract(attributes, '$.manager_id') = ? ORDER BY id`, managerID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	states := make([]sessionstore.OutputState, 0, want)
	for rows.Next() {
		var state sessionstore.OutputState
		if err := rows.Scan(&state); err != nil {
			return nil
		}

		states = append(states, state)
	}
	if rows.Err() != nil || len(states) != want {
		return nil
	}

	return states
}

func collectDurableCLIOutputs(t *testing.T, terminal *ctl.Client, count int) []Event {
	t.Helper()
	outputs := make([]Event, 0, count)
	for len(outputs) < count {
		outputs = append(outputs, waitForDelayedCLIEvent(t, terminal))
	}

	return outputs
}

func normalizeCLIEvents(events []Event) []Event {
	for i := range events {
		events[i].Generation = 0
		events[i].AfterOutputID = 0
	}

	return events
}

func sendDurableCLICommand(t *testing.T, terminal *ctl.Client, sessionID int64, text string) SendResult {
	t.Helper()
	var result SendResult
	require.NoError(t, terminal.Call(t.Context(), OpChatSend, SendParams{SessionID: sessionID, Text: text}, &result))

	return result
}

func waitForCLIType(t *testing.T, terminal *ctl.Client, eventType string) Event {
	t.Helper()
	for {
		event := waitForDelayedCLIEvent(t, terminal)
		if event.Type == eventType {
			return event
		}
	}
}

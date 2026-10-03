package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type scheduleBoundaryCommand uint8

const (
	deliverSubagentTick scheduleBoundaryCommand = iota
	deliverSubagentFresh
	deliverStoppedRoot
	stopRootAgain
	deliverDuplicateRoot
	deliverPendingResult
)

type scheduleBoundaryModel struct {
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootClaimed      bool
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

type scheduleBoundaryObservation struct {
	applied          bool
	errored          bool
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

// Parentage, runner admission and status transitions are daemon-owned protocol state.
func TestHarnessModel_ScheduleCapabilityBoundary(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			return &llmwire.Response{Text: "scheduled turn completed"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()
	h.startInboxWake()
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(rootID)
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	subagentID := createScheduleBoundarySubagent(t, h)

	model := scheduleBoundaryModel{
		rootStatus: sessionstore.SessionStatusStopped, subagentStatus: sessionstore.SessionStatusCompleted,
	}
	commands := []scheduleBoundaryCommand{
		deliverSubagentTick,
		deliverSubagentFresh,
		deliverStoppedRoot,
		stopRootAgain,
		deliverDuplicateRoot,
		deliverPendingResult,
	}

	for _, command := range commands {
		expectedApplied, expectedError := model.step(command)
		actual := applyScheduleBoundaryCommand(t, h, rootID, subagentID, command)
		assert.Equal(t, expectedApplied, actual.applied, "command %d applied", command)
		assert.Equal(t, expectedError, actual.errored, "command %d error", command)
		assert.Equal(t, model.rootStatus, actual.rootStatus, "command %d root status", command)
		assert.Equal(t, model.rootRuns, actual.rootRuns, "command %d root runs", command)
		assert.Equal(t, model.rootPending, actual.rootPending, "command %d root pending", command)
		assert.Equal(t, model.subagentStatus, actual.subagentStatus, "command %d subagent status", command)
		assert.Equal(t, model.subagentMessages, actual.subagentMessages, "command %d subagent messages", command)
	}
}

func (m *scheduleBoundaryModel) step(command scheduleBoundaryCommand) (bool, bool) {
	switch command {
	case deliverSubagentTick, deliverSubagentFresh:
		return false, false
	case deliverStoppedRoot:
		if m.rootClaimed {
			return false, false
		}

		m.rootClaimed = true
		m.rootStatus = sessionstore.SessionStatusCompleted
		m.rootRuns++
		return true, false
	case stopRootAgain:
		m.rootStatus = sessionstore.SessionStatusStopped
		return false, false
	case deliverDuplicateRoot:
		return false, false
	case deliverPendingResult:
		m.rootPending++
		return true, false
	default:
		panic("unknown schedule boundary command")
	}
}

func applyScheduleBoundaryCommand(
	t *testing.T,
	h *subagentHarness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) scheduleBoundaryObservation {
	t.Helper()

	applied, err := executeScheduleBoundaryCommand(t, h, rootID, subagentID, command)
	wantPending := 0

	if command == deliverPendingResult {
		wantPending = 1
	}

	require.Eventually(t, func() bool {
		pending, pendingErr := h.sessStore.ListPending(t.Context(), rootID)

		return pendingErr == nil && len(pending) == wantPending &&
			!h.mgr.HasActiveLoop(rootID) && !h.mgr.HasActiveLoop(subagentID)
	}, time.Second, 10*time.Millisecond)
	root, loadRootErr := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, loadRootErr)
	subagent, loadSubagentErr := h.sessStore.GetSession(t.Context(), subagentID)
	require.NoError(t, loadSubagentErr)
	pending, pendingErr := h.sessStore.ListPending(t.Context(), rootID)
	require.NoError(t, pendingErr)

	return scheduleBoundaryObservation{
		applied:          applied,
		errored:          err != nil,
		rootStatus:       root.Status,
		rootRuns:         countToolResultsFor(h.parentMessages(rootID), tool.IDSchedule),
		rootPending:      len(pending),
		subagentStatus:   subagent.Status,
		subagentMessages: len(h.parentMessages(subagentID)),
	}
}

func executeScheduleBoundaryCommand(
	t *testing.T,
	h *subagentHarness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) (bool, error) {
	t.Helper()

	switch command {
	case deliverSubagentTick:
		return enqueueScheduledInput(

			t.Context(), h.mgr.store,

			subagentID,
			"schedule:model:subagent-tick",
			"legacy task",
			false,
		)
	case deliverSubagentFresh:
		return enqueueScheduledInput(

			t.Context(), h.mgr.store,

			subagentID,
			"schedule:model:subagent-fresh",
			"legacy fresh task",
			true,
		)
	case deliverStoppedRoot, deliverDuplicateRoot:
		return enqueueScheduledInput(t.Context(), h.mgr.store, rootID, "schedule:model:root", "scheduled task", false)
	case stopRootAgain:
		return false, h.mgr.sendToSession(t.Context(), rootID, "/stop")
	case deliverPendingResult:
		applied, err := enqueueCallResult(

			t.Context(), h.mgr.store,

			rootID,
			"missing-call",
			tool.IDSleep,
			"must stay stopped",
		)

		return applied, err
	default:
		t.Fatalf("unknown command %d", command)

		return false, nil
	}
}

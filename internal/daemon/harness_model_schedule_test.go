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
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

type scheduleBoundaryObservation struct {
	applied          bool
	errored          bool
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
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
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(rootID)
	require.NoError(t, h.mgr.Stop(t.Context(), rootID, 0))
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
		return false, true
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

	require.Eventually(t, func() bool {
		return !h.mgr.HasActiveLoop(rootID) && !h.mgr.HasActiveLoop(subagentID)
	}, time.Second, 10*time.Millisecond)
	root, loadRootErr := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, loadRootErr)
	subagent, loadSubagentErr := h.sessStore.GetSession(t.Context(), subagentID)
	require.NoError(t, loadSubagentErr)

	return scheduleBoundaryObservation{
		applied:          applied,
		errored:          err != nil,
		rootStatus:       root.Status,
		rootRuns:         countToolResultsFor(h.parentMessages(rootID), tool.IDSchedule),
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
		return h.mgr.DeliverScheduleTick(t.Context(), subagentID, "schedule:model:subagent-tick", "legacy task")
	case deliverSubagentFresh:
		return h.mgr.DeliverFreshSchedule(t.Context(), subagentID, "schedule:model:subagent-fresh", "legacy fresh task")
	case deliverStoppedRoot, deliverDuplicateRoot:
		return h.mgr.DeliverScheduleTick(t.Context(), rootID, "schedule:model:root", "scheduled task")
	case stopRootAgain:
		return false, h.mgr.Stop(t.Context(), rootID, 0)
	case deliverPendingResult:
		_, err := h.mgr.DeliverPendingCallResult(
			t.Context(), rootID, "missing-call", tool.IDSleep, "must stay stopped",
		)

		return false, err
	default:
		t.Fatalf("unknown command %d", command)

		return false, nil
	}
}

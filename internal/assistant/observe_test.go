package assistant

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantObservationDecisions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   sessionstore.InputState
		status  sessionstore.SessionStatus
		busy    bool
		next    int64
		answer  string
		final   bool
		kind    string
		text    string
		settled bool
	}{
		{"pending", sessionstore.InputStatePending, sessionstore.SessionStatusActive, false, 0, "", false, "", "", false},
		{"pending stopped", sessionstore.InputStatePending, sessionstore.SessionStatusStopped, true, 0, "", false, "failed", "stopped", true},
		{"pending killed", sessionstore.InputStatePending, sessionstore.SessionStatusKilled, false, 0, "", false, "failed", "killed", true},
		{"pending error", sessionstore.InputStatePending, sessionstore.SessionStatusError, false, 0, "", false, "failed", "run failure", true},
		{"cancelled", sessionstore.InputStateCancelled, sessionstore.SessionStatusStopped, true, 0, "", false, "failed", "stopped", true},
		{"background", sessionstore.InputStateAccepted, sessionstore.SessionStatusCompleted, true, 0, "yield", true, "", "", false},
		{"answered", sessionstore.InputStateAccepted, sessionstore.SessionStatusCompleted, false, 0, "final", true, "answered", "final", true},
		{"superseded", sessionstore.InputStateAccepted, sessionstore.SessionStatusCompleted, false, 42, "", false, "", "", true},
		{"empty stopped", sessionstore.InputStateAccepted, sessionstore.SessionStatusStopped, false, 0, "", false, "failed", "stopped", true},
		{"empty error", sessionstore.InputStateAccepted, sessionstore.SessionStatusError, false, 0, "", false, "failed", "run failure", true},
		{"paused in a tool call", sessionstore.InputStateAccepted, sessionstore.SessionStatusCompleted, false, 0, "interim", false, "", "", false},
		{"answered then stopped", sessionstore.InputStateAccepted, sessionstore.SessionStatusStopped, false, 0, "final", true, "failed", "stopped\n\nBefore that it said:\nfinal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := window{state: tc.input, status: tc.status, nextUser: tc.next, answer: tc.answer, settled: tc.final}
			kind, text, settled := decide(w, tc.busy, errors.New("run failure"))
			assert.Equal(t, tc.kind, kind)
			assert.Equal(t, tc.text, text)
			assert.Equal(t, tc.settled, settled)
		})
	}
}

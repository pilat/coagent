package telegram

import (
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrchestrateCommand(t *testing.T) {
	for _, tc := range []struct {
		name, input, reply, operation string
		on, changed                   bool
		focus                         int64
		err                           error
	}{
		{"on", "/orchestrate on", "#12 is now an orchestrator: it can list, message and start your sessions.", "on", false, true, 12, nil},
		{"already on", "/orchestrate on", "#12 is already an orchestrator.", "on", true, false, 12, nil},
		{"off", "/orchestrate off", "#12 is no longer an orchestrator.", "off", true, true, 12, nil},
		{"already off", "/orchestrate off", "#12 is not an orchestrator.", "off", false, false, 12, nil},
		{"bare on", "/orchestrate", "#12: orchestrator on.", "read", true, false, 12, nil},
		{"bare off", "/orchestrate", "#12: orchestrator off. Use /orchestrate on to enable.", "read", false, false, 12, nil},
		{"uppercase", "/orchestrate ON", "#12 is now an orchestrator: it can list, message and start your sessions.", "on", false, true, 12, nil},
		{"trim", "/orchestrate   off  ", "#12 is no longer an orchestrator.", "off", true, true, 12, nil},
		{"usage", "/orchestrate maybe", "Usage: /orchestrate on|off.", "", false, false, 12, nil},
		{"no focus", "/orchestrate on", noFocusHint, "", false, false, 0, nil},
		{"read error", "/orchestrate", "#12 is not a live session", "read", false, false, 12, errors.New("#12 is not a live session")},
		{"write error", "/orchestrate on", "#12 is not a live session", "on", false, false, 12, errors.New("#12 is not a live session")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var messages []string
			ctrl := &fakeController{
				focus: tc.focus, orchestrator: tc.on,
				orchestratorChanged: tc.changed, orchestratorErr: tc.err,
			}
			m := newCmdTestManager(t, ctrl, &messages)
			m.handleChatText(t.Context(), normalizeTextCommand(tc.input))
			assert.Equal(t, []string{tc.reply}, messages)
			assert.Empty(t, ctrl.messageCalls)
			chat := strconv.FormatInt(harnessChatID, 10)
			if tc.operation == "read" {
				assert.Equal(t, []orchestratorRead{{chat, tc.focus}}, ctrl.orchestratorReads)
			} else {
				assert.Empty(t, ctrl.orchestratorReads)
			}
			if tc.operation == "on" || tc.operation == "off" {
				assert.Equal(t, []orchestratorWrite{{chat, tc.focus, tc.operation == "on"}}, ctrl.orchestratorWrites)
			} else {
				assert.Empty(t, ctrl.orchestratorWrites)
			}
		})
	}
}

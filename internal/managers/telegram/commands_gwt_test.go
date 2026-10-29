package telegram

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestParseGwtCommand(t *testing.T) {
	tests := []struct {
		text     string
		wantName string
		wantOK   bool
	}{
		{text: "/gwt api", wantName: "api", wantOK: true},
		{text: "/gwt   spaced-name  ", wantName: "spaced-name", wantOK: true},
		{text: "/gwt", wantName: "", wantOK: true},
		{text: "/gwtx", wantName: "", wantOK: false},
		{text: "/new api", wantName: "", wantOK: false},
		{text: "hello", wantName: "", wantOK: false},
	}

	for _, tt := range tests {
		name, ok := parseGwtCommand(tt.text)
		assert.Equal(t, tt.wantOK, ok, tt.text)
		assert.Equal(t, tt.wantName, name, tt.text)
	}
}

func TestGwtDispatchCreatesWorktreeSession(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, listSessions: []controllerapi.SessionInfo{{
		ID: 42, WorkDir: "/repo/work", Attributes: map[string]any{"manager_id": "telegram-main"},
	}}}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/gwt api")
	require.Len(t, ctrl.createSessionCalls, 1)
	assert.Equal(t, controllerapi.SessionCreateData{
		WorkDir: "/repo/work", WorktreeName: "api", Attributes: map[string]any{"channel": telegramChannel},
	}, ctrl.createSessionCalls[0])
	assert.Empty(t, ctrl.messageCalls)
	assert.Empty(t, messages)
	assert.Equal(t, int64(1), ctrl.focus)
}

func TestGwtFailureSendsOneErrorAndKeepsSelection(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, createSessionErr: errors.New("git failed")}
	m := newCmdTestManager(t, ctrl, &messages)
	m.workDirs[42] = "/repo/work"
	m.handleChatText(t.Context(), "/gwt api")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "git failed")
	assert.Equal(t, int64(42), ctrl.focus)
}

func TestGwtBareShowsUsageAndAssistantRefuses(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/gwt")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "Usage: /gwt")
	ctrl.focus = 0
	m.handleChatText(t.Context(), "/gwt api")
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1], "Switch to a work session")
	assert.Empty(t, ctrl.createSessionCalls)
}

package telegram

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
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
	ctrl := &fakeController{}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
		workDirs:   map[int64]string{42: "/repo/work"},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/gwt api")

	require.Len(t, ctrl.createSessionCalls, 1)
	assert.Equal(t, controllerapi.SessionCreateData{
		WorkDir:      "/repo/work",
		WorktreeName: "api",
		Attributes:   map[string]any{"channel": telegramChannel},
	}, ctrl.createSessionCalls[0])
	assert.Empty(t, ctrl.messageCalls, "/gwt must not be steered to the model")
}

func TestGwtEditsProgressIntoCreatedWithProjectName(t *testing.T) {
	var messages []string
	ctrl := &fakeController{
		listSessions: []controllerapi.SessionInfo{{
			ID: 1, ProjectName: "clone-9f3e21ab/api", WorkDir: "/wt/clone-9f3e21ab/api",
		}},
	}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
		workDirs:   map[int64]string{42: "/repo/work"},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/gwt api")

	require.Len(t, messages, 2, "the pending bubble must be edited, not left dangling")
	assert.Contains(t, messages[0], "Creating worktree")
	assert.Contains(t, messages[1], "Worktree created")
	assert.Contains(t, messages[1], "clone-9f3e21ab/api",
		"the fork must be reported under its registered project name")
	assert.Contains(t, messages[1], "#1")
}

func TestGwtFailureEditsProgressIntoError(t *testing.T) {
	var messages []string
	ctrl := &fakeController{createSessionErr: errors.New("boom")}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
		workDirs:   map[int64]string{42: "/repo/work"},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/gwt api")

	require.Len(t, messages, 2, "the pending bubble must turn into the failure")
	assert.Contains(t, messages[0], "Creating worktree")
	assert.Contains(t, messages[1], "/gwt failed")
}

func TestGwtBareShowsUsage(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
		workDirs:   map[int64]string{42: "/repo/work"},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/gwt")

	assert.Empty(t, ctrl.createSessionCalls, "bare /gwt creates nothing")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "Usage")
}

func TestGwtInServiceTopicSteersToSession(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := &Manager{
		cfg:            config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: telegramMessageRecorder(t, &messages)},
		serviceTopicID: 7,
	}

	m.handleServiceTopicMessage(context.Background(), "/gwt api")

	assert.Empty(t, ctrl.createSessionCalls, "/gwt in the service topic must not fork")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "session topic")
}

package telegram

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func newCmdTestManager(t *testing.T, ctrl *fakeController, messages *[]string) *Manager {
	t.Helper()
	m, err := New(config.ManagerEntry{
		ID: "telegram-main", BotToken: "test-token", AllowedUserIDs: []int64{harnessChatID},
	}, nil, ctrl)
	require.NoError(t, err)
	m.httpClient = &http.Client{Transport: telegramMessageRecorder(t, messages)}
	return m
}

func TestParseNewCommand(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{in: "/new", wantName: "", wantOK: true},
		{in: "/new посты", wantName: "посты", wantOK: true},
		{in: "/new  мои посты ", wantName: "мои посты", wantOK: true}, // spaces preserved inside, trimmed at edges
		{in: "/new ", wantName: "", wantOK: true},
		{in: "/spawn", wantOK: false},
		{in: "hello", wantOK: false},
		{in: "/newx", wantOK: false}, // must not match a longer command
		{in: "/news feed", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			name, ok := parseNewCommand(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			if ok {
				assert.Equal(t, tc.wantName, name)
			}
		})
	}
}

func TestParseNewCommand_AtBotStrippedUpstream(t *testing.T) {
	// normalizeTextCommand strips @bot before dispatch reaches parseNewCommand.
	name, ok := parseNewCommand(normalizeTextCommand("/new@mybot посты"))
	require.True(t, ok)
	assert.Equal(t, "посты", name)
}

func TestFormatAge(t *testing.T) {
	assert.Equal(t, "0m", formatAge(0))
	assert.Equal(t, "0m", formatAge(-5*time.Minute))
	assert.Equal(t, "30m", formatAge(30*time.Minute))
	assert.Equal(t, "1h", formatAge(90*time.Minute))
	assert.Equal(t, "23h", formatAge(23*time.Hour))
	assert.Equal(t, "1d", formatAge(25*time.Hour))
	assert.Equal(t, "3d", formatAge(72*time.Hour))
}

func TestRelativeAge_NilIsNew(t *testing.T) {
	assert.Equal(t, "new", relativeAge(nil))

	recent := time.Now().Add(-10 * time.Minute)
	assert.Equal(t, "10m", relativeAge(&recent))
}

func TestHandleNew_CreatesAndLaunches(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/new посты")

	require.Len(t, ctrl.createProjectCalls, 1)
	assert.Equal(t, "посты", ctrl.createProjectCalls[0].Name)
	require.Len(t, ctrl.createSessionCalls, 1)
	assert.Equal(t, "/root/посты", ctrl.createSessionCalls[0].WorkDir)
	assert.Equal(t, "telegram", ctrl.createSessionCalls[0].Attributes["channel"])
	assert.Equal(t, int64(1), ctrl.focus)
	assert.Empty(t, messages, "the delivery loop owns the switch confirmation")
}

func TestHandleNew_ErrorReplyNoLaunch(t *testing.T) {
	var messages []string
	ctrl := &fakeController{createProjectErr: errors.New("project name is invalid")}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/new .bad")
	assert.Empty(t, ctrl.createSessionCalls)
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "project name is invalid")
}

func TestHandleNew_ListsProjectsWithAge(t *testing.T) {
	var messages []string
	old := time.Now().Add(-2 * time.Hour)
	ctrl := &fakeController{recentProjects: []controllerapi.RecentProjectInfo{
		{ID: 5, Name: "посты"}, {ID: 3, Name: "blog", LastActivity: &old},
	}}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/new")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "1. посты · new")
	assert.Contains(t, messages[0], "2. blog · 2h")
	assert.Empty(t, ctrl.createSessionCalls)

	ctrl.recentProjects = nil
	m.handleChatText(t.Context(), "/new")
	assert.Contains(t, messages[1], "No projects yet")
	ctrl.recentProjectsErr = errors.New("unavailable")
	m.handleChatText(t.Context(), "/new")
	assert.Contains(t, messages[2], "Failed to list projects")
}

func TestSpawn_PathCreatesAndBareListsFavorites(t *testing.T) {
	var messages []string
	ctrl := &fakeController{
		listDirResult: &controllerapi.FsListDirResultData{Favorites: []string{"/repo/one", "/repo/two"}},
	}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/spawn")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "Usage: /spawn")
	assert.Contains(t, messages[0], "/repo/one\n/repo/two")
	m.handleChatText(t.Context(), "/spawn /some path/repo")
	require.Len(t, ctrl.createSessionCalls, 1)
	assert.Equal(t, "/some path/repo", ctrl.createSessionCalls[0].WorkDir)
	assert.Equal(t, int64(1), ctrl.focus)
	assert.Len(t, messages, 1)
}

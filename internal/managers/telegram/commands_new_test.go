package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func newCmdTestManager(t *testing.T, ctrl *fakeController, messages *[]string) *Manager {
	t.Helper()

	enabled := true

	return &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     -100123,
			AllowedUserIDs:   []int64{42},
			SendChunkDelayMS: 0,
			PollTimeoutSec:   30,
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: telegramMessageRecorder(t, messages)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
		serviceTopicID: 6344,
	}
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

func TestBuildNewPickerKeyboard(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	projects := []controllerapi.RecentProjectInfo{
		{ID: 5, Name: "посты", LastActivity: nil},
		{ID: 3, Name: "blog", LastActivity: &old},
	}

	kb := buildNewPickerKeyboard(projects, 10, true)

	require.Len(t, kb, 3) // two project rows + one pagination row

	assert.Equal(t, "newpick:5", kb[0][0].CallbackData)
	assert.Equal(t, "посты · new", kb[0][0].Text)
	assert.Equal(t, "newpick:3", kb[1][0].CallbackData)
	assert.True(t, strings.HasPrefix(kb[1][0].Text, "blog · "), "button shows name and age")

	// offset 10 with hasMore → both back and more.
	pag := kb[2]
	require.Len(t, pag, 2)
	assert.Equal(t, "newpage:0", pag[0].CallbackData)
	assert.Equal(t, "newpage:20", pag[1].CallbackData)
}

func TestBuildNewPickerKeyboard_FirstPageNoBack(t *testing.T) {
	projects := []controllerapi.RecentProjectInfo{{ID: 1, Name: "a"}}
	kb := buildNewPickerKeyboard(projects, 0, false)
	require.Len(t, kb, 1) // one project row, no pagination
}

func TestHandleNew_CreatesAndLaunches(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleServiceTopicMessage(context.Background(), "/new посты")

	require.Len(t, ctrl.createProjectCalls, 1)
	assert.Equal(t, "посты", ctrl.createProjectCalls[0].Name)
	require.Len(t, ctrl.createSessionCalls, 1)
	assert.Equal(t, "/root/посты", ctrl.createSessionCalls[0].WorkDir)
}

func TestHandleNew_ErrorReplyNoLaunch(t *testing.T) {
	var messages []string
	ctrl := &fakeController{createProjectErr: errors.New(`project name must not start with "."`)}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleServiceTopicMessage(context.Background(), "/new .bad")

	require.Len(t, ctrl.createProjectCalls, 1)
	assert.Empty(t, ctrl.createSessionCalls, "bad name must not launch a session")
	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "must not start")
}

func TestNewInSessionTopicRedirects(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/new посты")

	assert.Empty(t, ctrl.createProjectCalls)
	assert.Empty(t, ctrl.messageCalls, "/new must not be sent to the LLM")
	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "service topic")
}

func TestHandleNewPicker_Empty(t *testing.T) {
	var messages []string
	ctrl := &fakeController{recentProjects: nil}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleNewPicker(context.Background(), 0, 0)

	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "No projects yet")
}

func TestHandleNewPicker_ListError(t *testing.T) {
	var messages []string
	ctrl := &fakeController{recentProjectsErr: errors.New("boom")}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleNewPicker(context.Background(), 0, 0)

	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "Failed to list projects")
}

func TestHandleCallbackNewPage_RepaginatesPicker(t *testing.T) {
	var messages []string
	projects := make([]controllerapi.RecentProjectInfo, 15)
	for i := range projects {
		projects[i] = controllerapi.RecentProjectInfo{ID: int64(i + 1), Name: "p"}
	}

	ctrl := &fakeController{recentProjects: projects}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleCallback(context.Background(), &telegramCallbackData{
		ID:   "cb-page",
		From: &telegramUser{ID: 42},
		Data: "newpage:10",
		Message: &telegramCallbackMeta{
			Chat:            telegramChat{ID: -100123},
			MessageID:       5, // >0 → edits the picker in place
			MessageThreadID: 6344,
		},
	})

	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "Projects")
}

func TestHandleCallbackNewPick_VanishedProject(t *testing.T) {
	var messages []string
	ctrl := &fakeController{recentProjects: []controllerapi.RecentProjectInfo{{ID: 1, Name: "a"}}}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleCallback(context.Background(), &telegramCallbackData{
		ID:   "cb-gone",
		From: &telegramUser{ID: 42},
		Data: "newpick:999", // id absent from the (re-listed) projects
		Message: &telegramCallbackMeta{
			Chat:            telegramChat{ID: -100123},
			MessageID:       1,
			MessageThreadID: 6344,
		},
	})

	assert.Empty(t, ctrl.createProjectCalls)
	assert.Contains(t, strings.Join(messages, "\n"), "no longer available")
}

func TestHandleCallbackNewPick_RoutesThroughCreateProject(t *testing.T) {
	var messages []string
	ctrl := &fakeController{
		recentProjects: []controllerapi.RecentProjectInfo{
			{ID: 7, Name: "посты", Path: "/root/посты"},
		},
	}
	m := newCmdTestManager(t, ctrl, &messages)

	m.handleCallback(context.Background(), &telegramCallbackData{
		ID:   "cb-1",
		From: &telegramUser{ID: 42},
		Data: "newpick:7",
		Message: &telegramCallbackMeta{
			Chat:            telegramChat{ID: -100123},
			MessageID:       1,
			MessageThreadID: 6344,
		},
	})

	require.Len(t, ctrl.createProjectCalls, 1)
	assert.Equal(t, "посты", ctrl.createProjectCalls[0].Name)
	require.Len(t, ctrl.createSessionCalls, 1)
}

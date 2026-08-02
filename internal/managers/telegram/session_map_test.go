package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeController struct {
	lastSetAttrs  controllerapi.SessionSetAttributesData
	listSessions  []controllerapi.SessionInfo
	listSkills    []controllerapi.ConfigSkillInfo
	listSchedules []controllerapi.ScheduleInfo
	scheduleCalls []controllerapi.ScheduleListData
	killCalls     []int64
	stopCalls     []int64
	messageCalls  []controllerapi.SessionMessageData

	createSessionCalls  []controllerapi.SessionCreateData
	createProjectCalls  []controllerapi.ProjectCreateData
	createProjectResult *controllerapi.ProjectCreateResultData
	createProjectErr    error
	recentProjects      []controllerapi.RecentProjectInfo
	recentProjectsErr   error

	listModels      []controllerapi.ConfigModelInfo
	listModelsCalls int
	setModelCalls   []controllerapi.SessionSetModelData
	setModelErr     error
	setAttrsErr     error
	setAttrsHook    func(controllerapi.SessionSetAttributesData)
}

func (f *fakeController) CreateSession(_ context.Context, data controllerapi.SessionCreateData) (int64, error) {
	f.createSessionCalls = append(f.createSessionCalls, data)
	return 0, nil
}

func (f *fakeController) CreateProject(
	_ context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	f.createProjectCalls = append(f.createProjectCalls, data)

	if f.createProjectErr != nil {
		return nil, f.createProjectErr
	}

	if f.createProjectResult != nil {
		return f.createProjectResult, nil
	}

	return &controllerapi.ProjectCreateResultData{ID: 1, Name: data.Name, Path: "/root/" + data.Name}, nil
}

func (f *fakeController) ListRecentProjects(context.Context) (*controllerapi.ProjectListResultData, error) {
	if f.recentProjectsErr != nil {
		return nil, f.recentProjectsErr
	}

	return &controllerapi.ProjectListResultData{Projects: f.recentProjects}, nil
}

func (f *fakeController) ListSessions(context.Context) ([]controllerapi.SessionInfo, error) {
	return f.listSessions, nil
}

func (f *fakeController) KillSession(_ context.Context, data controllerapi.SessionKillData) error {
	f.killCalls = append(f.killCalls, data.SessionID)
	return nil
}

func (f *fakeController) StopSession(_ context.Context, data controllerapi.SessionStopData) error {
	f.stopCalls = append(f.stopCalls, data.SessionID)
	return nil
}

func (f *fakeController) ClearSession(context.Context, controllerapi.SessionClearData) (int64, error) {
	return 0, nil
}

func (f *fakeController) SendSessionMessage(_ context.Context, data controllerapi.SessionMessageData) error {
	f.messageCalls = append(f.messageCalls, data)
	return nil
}

func (f *fakeController) SetSessionModel(_ context.Context, data controllerapi.SessionSetModelData) error {
	f.setModelCalls = append(f.setModelCalls, data)
	return f.setModelErr
}

func (f *fakeController) SetSessionAttributes(_ context.Context, data controllerapi.SessionSetAttributesData) error {
	f.lastSetAttrs = data
	if f.setAttrsHook != nil {
		f.setAttrsHook(data)
	}

	return f.setAttrsErr
}

func (f *fakeController) ListDir(
	context.Context,
	controllerapi.FsListDirData,
) (*controllerapi.FsListDirResultData, error) {
	return nil, nil
}

func (f *fakeController) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	f.listModelsCalls++
	return &controllerapi.ConfigModelsResultData{Models: f.listModels}, nil
}

func (f *fakeController) ListSkills(
	context.Context,
	controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	return &controllerapi.ConfigSkillsResultData{Skills: f.listSkills}, nil
}

func (f *fakeController) ListSchedules(
	_ context.Context,
	data controllerapi.ScheduleListData,
) (*controllerapi.ScheduleListResultData, error) {
	f.scheduleCalls = append(f.scheduleCalls, data)
	return &controllerapi.ScheduleListResultData{Schedules: f.listSchedules}, nil
}

func (f *fakeController) SubscribeAll() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification)
	close(ch)
	return ch
}

func (f *fakeController) UnsubscribeAll(ch <-chan controllerapi.SessionNotification) {}

func TestHandleNotification_SessionClearedRemapsTopic(t *testing.T) {
	enabled := true
	ctrl := &fakeController{}

	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     -100123,
			SendChunkDelayMS: 0,
			PollTimeoutSec:   30,
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}

	m.registerTopic(1, 5001)
	m.setWorkDir(1, "/tmp/old")

	m.handleNotification(context.Background(), controllerapi.SessionNotification{
		SessionID: 1,
		Notification: sessionevent.Notification{
			Type:         sessionevent.NotifySessionCleared,
			OldSessionID: 1,
			NewSessionID: 2,
			WorkDir:      "/tmp/new",
			Attributes:   map[string]any{"language": "ru"},
		},
	})

	_, oldExists := m.getTopicBySessionID(1)
	newTopic, newExists := m.getTopicBySessionID(2)
	assert.False(t, oldExists)
	require.True(t, newExists)
	assert.Equal(t, int64(5001), newTopic)
	assert.Equal(t, int64(2), ctrl.lastSetAttrs.SessionID)
	assert.Equal(t, int64(5001), ctrl.lastSetAttrs.Attributes["telegram_topic_id"])
	assert.Equal(t, "ru", ctrl.lastSetAttrs.Attributes["language"], "binding must preserve unrelated attributes")
}

func TestCreateTopicForSessionPersistsBeforeCaching(t *testing.T) {
	ctrl := &fakeController{}
	var manager *Manager
	var mappedDuringPersist bool
	ctrl.setAttrsHook = func(data controllerapi.SessionSetAttributesData) {
		_, mappedDuringPersist = manager.getTopicBySessionID(data.SessionID)
	}

	manager = &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: -100123},
		controller: ctrl,
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7001}}`), nil
		})},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}

	topicID, err := manager.createTopicForSession(
		context.Background(),
		41,
		"/tmp/project",
		"project",
		map[string]any{"channel": "telegram"},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(7001), topicID)
	assert.False(t, mappedDuringPersist, "in-memory routing must not lead its durable source of truth")
	assert.Equal(t, "telegram", ctrl.lastSetAttrs.Attributes["channel"])
	assert.Equal(t, int64(7001), ctrl.lastSetAttrs.Attributes["telegram_topic_id"])

	cachedTopic, ok := manager.getTopicBySessionID(41)
	require.True(t, ok)
	assert.Equal(t, topicID, cachedTopic)
}

func TestCreateTopicForSessionCompensatesPersistenceFailure(t *testing.T) {
	ctrl := &fakeController{setAttrsErr: errors.New("database unavailable")}
	methods := make([]string, 0, 2)
	manager := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: -100123},
		controller: ctrl,
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			methods = append(methods, method)
			if method == "createForumTopic" {
				return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7002}}`), nil
			}

			return telegramResponse(req, `{"ok":true,"result":true}`), nil
		})},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}

	_, err := manager.createTopicForSession(context.Background(), 42, "/tmp/project", "project", nil)
	require.ErrorContains(t, err, "persist topic 7002")
	assert.Equal(t, []string{"createForumTopic", "deleteForumTopic"}, methods)

	_, mapped := manager.getTopicBySessionID(42)
	assert.False(t, mapped, "an unpersisted topic must never become routable")
	assert.NotContains(t, manager.workDirs, int64(42))
}

func TestHandleNotificationClearKeepsOldMappingWhenPersistenceFails(t *testing.T) {
	ctrl := &fakeController{setAttrsErr: errors.New("database unavailable")}
	manager := &Manager{
		cfg:            config.ManagerEntry{BotToken: "token", TargetChatID: -100123},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}
	manager.registerTopic(1, 5001)
	manager.setWorkDir(1, "/tmp/old")

	manager.handleNotification(context.Background(), controllerapi.SessionNotification{
		SessionID: 1,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifySessionCleared, OldSessionID: 1, NewSessionID: 2, WorkDir: "/tmp/new",
		},
	})

	oldTopic, oldExists := manager.getTopicBySessionID(1)
	_, newExists := manager.getTopicBySessionID(2)
	require.True(t, oldExists)
	assert.Equal(t, int64(5001), oldTopic)
	assert.False(t, newExists, "failed durability must not be presented as a successful remap")
}

func okTelegramRoundTrip(req *http.Request) (*http.Response, error) {
	payload := `{"ok":true,"result":{"message_id":123}}`
	return telegramResponse(req, payload), nil
}

func telegramResponse(req *http.Request, payload string) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(payload)),
		Header:     make(http.Header),
		Request:    req,
	}

	return resp
}

func TestServiceTopicPath(t *testing.T) {
	m := &Manager{cfg: config.ManagerEntry{TargetChatID: -100123}}

	home := t.TempDir()
	restoreHome := coagenthome.Override(home)
	want := filepath.Join(
		home,
		coagenthome.DirName,
		fmt.Sprintf(coagenthome.TelegramServiceFilePattern, m.cfg.TargetChatID),
	)
	assert.Equal(t, want, m.serviceTopicPath())
	restoreHome()

	restoreEmpty := coagenthome.Override("")
	defer restoreEmpty()
	assert.Empty(t, m.serviceTopicPath())
}

func TestEnsureServiceTopicPersistsBeforeReturning(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

	methods := make([]string, 0, 1)
	m := &Manager{
		cfg: config.ManagerEntry{
			BotToken:     "token",
			TargetChatID: -100123,
		},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			methods = append(methods, filepath.Base(req.URL.Path))

			return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7003}}`), nil
		})},
	}

	topicID, err := m.ensureServiceTopic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(7003), topicID)
	assert.Equal(t, []string{"createForumTopic"}, methods)

	saved, err := m.loadServiceTopicID()
	require.NoError(t, err)
	assert.Equal(t, topicID, saved)
}

func TestEnsureServiceTopicCompensatesPersistenceFailure(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	methods := make([]string, 0, 2)
	m := &Manager{
		cfg: config.ManagerEntry{
			BotToken:     "token",
			TargetChatID: -100123,
		},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			methods = append(methods, method)
			if method == "createForumTopic" {
				// Loading observed a missing path. Turn its parent into a regular
				// file before persistence so the write fails deterministically,
				// including when tests run as root.
				require.NoError(t, os.WriteFile(
					filepath.Join(home, coagenthome.DirName),
					[]byte("not a directory"),
					0o600,
				))

				return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7004}}`), nil
			}

			return telegramResponse(req, `{"ok":true,"result":true}`), nil
		})},
	}

	_, err := m.ensureServiceTopic(context.Background())
	require.ErrorContains(t, err, "persist service topic 7004")
	assert.Equal(t, []string{"createForumTopic", "deleteForumTopic"}, methods)
}

func TestEnsureServiceTopicRejectsCorruptDurableRecord(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	dir := filepath.Join(home, coagenthome.DirName)
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, fmt.Sprintf(coagenthome.TelegramServiceFilePattern, -100123)),
		[]byte("not-json"),
		0o600,
	))

	called := false
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "token", TargetChatID: -100123},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true

			return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7005}}`), nil
		})},
	}

	_, err := m.ensureServiceTopic(context.Background())
	require.ErrorContains(t, err, "decode service topic file")
	assert.False(t, called, "a corrupt durable record must not create a duplicate remote topic")
}

func TestTopicIDFromAttributes(t *testing.T) {
	id, ok := topicIDFromAttributes(map[string]any{"telegram_topic_id": float64(42)})
	require.True(t, ok)
	assert.Equal(t, int64(42), id)

	_, ok = topicIDFromAttributes(nil)
	assert.False(t, ok)

	_, ok = topicIDFromAttributes(map[string]any{"telegram_topic_id": time.Now()})
	assert.False(t, ok)
}

func TestResolveSessionByTopicID_UsesMetadataFallback(t *testing.T) {
	enabled := true
	ctrl := &fakeController{
		listSessions: []controllerapi.SessionInfo{
			{
				ID:         96,
				WorkDir:    "/tmp/live",
				Status:     "completed",
				Attributes: map[string]any{"telegram_topic_id": int64(6344)},
			},
		},
	}

	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     -100123,
			SendChunkDelayMS: 0,
			PollTimeoutSec:   30,
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}

	sessionID, ok := m.resolveSessionByTopicID(context.Background(), 6344)
	require.True(t, ok)
	assert.Equal(t, int64(96), sessionID)

	cachedID, cached := m.getSessionByTopicID(6344)
	require.True(t, cached)
	assert.Equal(t, int64(96), cachedID)
}

func TestReconcileOnStartup_IgnoresKilledSessions(t *testing.T) {
	enabled := true
	killedAt := time.Now().UTC()
	ctrl := &fakeController{
		listSessions: []controllerapi.SessionInfo{
			{
				ID:         96,
				WorkDir:    "/tmp/live",
				Status:     "completed",
				Attributes: map[string]any{"telegram_topic_id": int64(6344)},
			},
			{
				ID:         95,
				WorkDir:    "/tmp/old",
				Status:     "active",
				KilledAt:   &killedAt,
				Attributes: map[string]any{"telegram_topic_id": int64(6344)},
			},
		},
	}

	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     -100123,
			SendChunkDelayMS: 0,
			PollTimeoutSec:   30,
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}

	require.NoError(t, m.reconcileOnStartup(context.Background()))

	sessionID, ok := m.getSessionByTopicID(6344)
	require.True(t, ok)
	assert.Equal(t, int64(96), sessionID)

	_, hasKilledMapping := m.getTopicBySessionID(95)
	assert.False(t, hasKilledMapping)
}

func TestHandleCallback_KillDispatchesController(t *testing.T) {
	enabled := true
	ctrl := &fakeController{}

	m := &Manager{
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
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
		serviceTopicID: 6344,
	}

	m.handleCallback(context.Background(), &telegramCallbackData{
		ID:   "cb-1",
		From: &telegramUser{ID: 42},
		Data: "kill:95",
		Message: &telegramCallbackMeta{
			Chat:            telegramChat{ID: -100123},
			MessageID:       1,
			MessageThreadID: 6344,
		},
	})

	require.Len(t, ctrl.killCalls, 1)
	assert.Equal(t, int64(95), ctrl.killCalls[0])
}

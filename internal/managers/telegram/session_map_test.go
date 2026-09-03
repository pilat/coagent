package telegram

import (
	"context"
	"crypto/sha256"
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
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeController struct {
	lastSetAttrs controllerapi.SessionSetAttributesData
	listSessions []controllerapi.SessionInfo
	listSkills   []controllerapi.ConfigSkillInfo
	messageCalls []controllerapi.SessionMessageData

	createSessionCalls  []controllerapi.SessionCreateData
	createSessionErr    error
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
	if f.createSessionErr != nil {
		return 0, f.createSessionErr
	}

	// Distinct positive IDs: handlers map the returned session for follow-ups.
	return int64(len(f.createSessionCalls)), nil
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

func (f *fakeController) Subscribe() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification)
	close(ch)
	return ch
}

func (f *fakeController) Unsubscribe(ch <-chan controllerapi.SessionNotification) {}

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
	m := &Manager{id: "telegram-main", cfg: config.ManagerEntry{TargetChatID: targetID(-100123)}}

	home := t.TempDir()
	restoreHome := coagenthome.Override(home)
	want := filepath.Join(
		home,
		coagenthome.DirName,
		"tg-service-manager-"+fmt.Sprintf("%x", sha256.Sum256([]byte(m.id)))+".json",
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
			TargetChatID: targetID(-100123),
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
			TargetChatID: targetID(-100123),
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
		cfg: config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true

			return telegramResponse(req, `{"ok":true,"result":{"message_thread_id":7005}}`), nil
		})},
	}

	_, err := m.ensureServiceTopic(context.Background())
	require.ErrorContains(t, err, "decode legacy service topic file")
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
				ID:      96,
				WorkDir: "/tmp/live",
				Status:  "completed",
				Attributes: map[string]any{
					"telegram_topic_id":                     int64(6344),
					controllerapi.SessionAttributeManagerID: "telegram-main",
				},
			},
		},
	}

	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     targetID(-100123),
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
				ID:      96,
				WorkDir: "/tmp/live",
				Status:  "completed",
				Attributes: map[string]any{
					"telegram_topic_id":                     int64(6344),
					controllerapi.SessionAttributeManagerID: "telegram-main",
				},
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
			TargetChatID:     targetID(-100123),
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
	ctrl := &fakeController{listSessions: []controllerapi.SessionInfo{{
		ID: 95, Status: "active", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "telegram-main",
		},
	}}}

	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:               "telegram-main",
			Enabled:          &enabled,
			BotToken:         "token",
			TargetChatID:     targetID(-100123),
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

	require.Equal(t, []controllerapi.SessionMessageData{
		{SessionID: 95, Message: commandKill},
	}, ctrl.messageCalls)
}

func TestHandleCallback_KillRejectsAForeignRetainedButton(t *testing.T) {
	enabled := true
	ctrl := &fakeController{listSessions: []controllerapi.SessionInfo{{
		ID: 95, Status: "active", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "telegram-secondary",
		},
	}}}
	m := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID: "telegram-main", Enabled: &enabled, BotToken: "token",
			TargetChatID: targetID(-100123), AllowedUserIDs: []int64{42},
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: roundTripFunc(okTelegramRoundTrip)},
		sessionToTopic: map[int64]int64{}, topicToSession: map[int64]int64{},
		navPaths: map[int64]string{}, pathToNav: map[string]int64{}, workDirs: map[int64]string{},
		serviceTopicID: 6344,
	}

	m.handleCallback(context.Background(), &telegramCallbackData{
		ID: "cb-foreign", From: &telegramUser{ID: 42}, Data: "kill:95",
		Message: &telegramCallbackMeta{
			Chat: telegramChat{ID: -100123}, MessageID: 1, MessageThreadID: 6344,
		},
	})

	assert.Empty(t, ctrl.messageCalls)
}

package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

const delayedTelegramManagerID = "telegram-delayed"

type delayedTelegramHarness struct {
	controller controllerapi.Controller
	sessions   sessionstore.Store
	service    daemon.Service
	workDir    string
	recorder   *delayedTelegramRecorder
}

// A real session can commit while Telegram is down. Starting the production
// renderer later must create its surface and deliver that pre-existing answer.
func TestHarnessScenario_DelayedTelegramManagerDrainsRealSessionOutput(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	sessionID := h.produceBeforeManager(t)
	h.waitForBacklog(t)

	manager, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", TargetChatID: targetID(harnessChatID),
	}, &config.UnifiedConfig{}, h.controller)
	require.NoError(t, err)
	manager.httpClient = &http.Client{Transport: h.recorder}
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })

	require.Eventually(t, func() bool {
		return h.recorder.hasMessage("delayed telegram answer")
	}, 5*time.Second, 10*time.Millisecond, "the delayed manager must render its persisted answer")
	require.Eventually(t, func() bool {
		status, statusErr := h.sessions.OutputQueueStatus(t.Context(), delayedTelegramManagerID)
		return statusErr == nil && status.Pending == 0
	}, 10*time.Second, 10*time.Millisecond, "the production telegram adapter must acknowledge every queued row")

	calls := h.recorder.snapshot()
	assert.True(t, hasTelegramMethod(calls, "createForumTopic"), "session delivery must establish a forum topic")
	assert.False(t, hasTelegramMessage(calls, "Task accepted"))
	assert.True(t, hasTelegramMessage(calls, "delayed telegram answer"))
	assert.NotZero(t, sessionID)
}

func newDelayedTelegramHarness(t *testing.T) *delayedTelegramHarness {
	t.Helper()
	home := t.TempDir()
	restoreHome := coagenthome.Override(home)
	t.Cleanup(restoreHome)
	require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

	dbPath := filepath.Join(home, "coagent.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))

	projects := daemon.NewStore(db)
	sessions := sessionstore.NewStore(db)
	workDir := filepath.Join(home, "project")
	cfg := &config.Config{Model: "fake-model", WorkDir: workDir}
	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessions, sessions, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(*config.Config) (llm.Client, error) {
			return delayedTelegramClient{}, nil
		}),
	)
	service := daemon.New(
		factory, projects, sessions, sessions, sessions, sessions, sessions, sessions, sessions,
		subagent.NewStore(db), subagent.NewTransactions(db),
		budget.New(sessions), sessions, schedule.NewService(schedule.NewStore(db)), cfg, nil, nil, nil,
	)
	t.Cleanup(func() { service.Shutdown(3 * time.Second) })
	controllers := daemon.NewController(service, cfg, nil, nil)

	return &delayedTelegramHarness{
		controller: controllers.ForManager(delayedTelegramManagerID), sessions: sessions,
		service: service, workDir: workDir, recorder: &delayedTelegramRecorder{nextTopicID: 7000},
	}
}

func (h *delayedTelegramHarness) produceBeforeManager(t *testing.T) int64 {
	t.Helper()
	sessionID, err := h.controller.CreateSession(t.Context(), controllerapi.SessionCreateData{
		WorkDir: h.workDir, Prompt: "finish before telegram starts", Model: "fake-model",
	})
	require.NoError(t, err)

	return sessionID
}

func (h *delayedTelegramHarness) waitForBacklog(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := h.sessions.OutputQueueStatus(t.Context(), delayedTelegramManagerID)
		return err == nil && status.Pending >= 2
	}, 10*time.Second, 10*time.Millisecond, "the real session must commit lifecycle and answer before manager start")
}

type delayedTelegramClient struct{}

func (delayedTelegramClient) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return &llmwire.Response{Text: "delayed telegram answer"}, nil
}

func (delayedTelegramClient) Model() string             { return "fake-model" }
func (delayedTelegramClient) APIKey() string            { return "" }
func (delayedTelegramClient) Close() error              { return nil }
func (delayedTelegramClient) Provider() string          { return "fake" }
func (delayedTelegramClient) ContextWindow() int        { return 200000 }
func (delayedTelegramClient) SetReasoningLevel(string)  {}
func (delayedTelegramClient) GetReasoningLevel() string { return "medium" }
func (delayedTelegramClient) SetSessionID(string)       {}

type delayedTelegramCall struct {
	Method string
	Text   string
}

type delayedTelegramRecorder struct {
	mu          sync.Mutex
	calls       []delayedTelegramCall
	nextTopicID int64
}

func (r *delayedTelegramRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	method := filepath.Base(request.URL.Path)
	if method == "getUpdates" {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s request: %w", method, err)
	}

	r.mu.Lock()
	r.calls = append(r.calls, delayedTelegramCall{Method: method, Text: payload.Text})
	if method == "createForumTopic" {
		r.nextTopicID++
	}
	topicID := r.nextTopicID
	r.mu.Unlock()

	result := `true`
	switch method {
	case "getMe":
		result = `{"id":42}`
	case "getChat":
		result = fmt.Sprintf(`{"id":%d,"type":"supergroup","is_forum":true}`, harnessChatID)
	case "getChatMember":
		result = `{"status":"administrator","can_manage_topics":true,"can_delete_messages":true}`
	case "createForumTopic":
		result = fmt.Sprintf(`{"message_thread_id":%d}`, topicID)
	case "sendMessage":
		result = fmt.Sprintf(`{"message_id":123,"message_thread_id":%d}`, topicID)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":` + result + `}`)),
		Request:    request,
	}, nil
}

func (r *delayedTelegramRecorder) hasMessage(text string) bool {
	return hasTelegramMessage(r.snapshot(), text)
}

func (r *delayedTelegramRecorder) snapshot() []delayedTelegramCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]delayedTelegramCall(nil), r.calls...)
}

func hasTelegramMethod(calls []delayedTelegramCall, method string) bool {
	for _, call := range calls {
		if call.Method == method {
			return true
		}
	}

	return false
}

func hasTelegramMessage(calls []delayedTelegramCall, text string) bool {
	for _, call := range calls {
		if call.Method == "sendMessage" && call.Text == text {
			return true
		}
	}

	return false
}

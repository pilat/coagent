package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const delayedTelegramManagerID = "telegram-delayed"

type delayedTelegramHarness struct {
	controller  controllerapi.Controller
	controllers controllerapi.ManagerControllerFactory
	sessions    *sessionstore.Store
	service     *daemon.Service
	workDir     string
	recorder    *delayedTelegramRecorder
}

// A selected session's committed output must survive the manager being offline.
func TestHarnessScenario_DelayedTelegramManagerDrainsRealSessionOutput(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	sessionID := h.produceBeforeManager(t)
	h.waitForBacklog(t, sessionID)
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), sessionID))

	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, sessionID)
	assert.True(t, hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf(
		"#%d project · finish before telegram starts\ndelayed telegram answer", sessionID)))
	assert.False(t, hasTelegramMessage(h.recorder.snapshot(), "Task accepted"))
	assert.NotZero(t, sessionID)
}

func newDelayedTelegramHarness(t *testing.T) *delayedTelegramHarness {
	return newDelayedTelegramHarnessWithClient(t, func(*config.Config) (llm.Client, error) {
		return delayedTelegramClient{}, nil
	})
}

func newDelayedTelegramHarnessWithClient(
	t *testing.T,
	clientFactory func(*config.Config) (llm.Client, error),
) *delayedTelegramHarness {
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

	sessions := sessionstore.NewStore(db)
	workDir := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(workDir, 0o700))
	cfg := &config.Config{Model: "fake-model", WorkDir: workDir}
	client, clientErr := clientFactory(cfg)
	var build sessionbuild.BuildInput
	if clientErr != nil {
		cfg.UnifiedConfig = &config.UnifiedConfig{Models: []config.ModelEntry{{ID: "fake-model", Provider: "missing"}}}
		build = sessionbuild.BuildInput{Config: cfg, Store: sessions, Resources: builtin.NewResources()}
	} else {
		build = scriptedBuildInput(
			t,
			cfg,
			sessions,
			nil,
			func(*config.Config) (llm.Client, error) { return client, nil },
		)
	}
	bus := sessionbus.New()
	progress := progressruntime.New(sessions)
	service := daemon.New(
		context.Background(),
		build,
		sessions,
		subagent.NewStore(db, sessions),
		budget.New(sessions),
		backgroundprocess.NewStore(db, sessions),
		progress,
		bus,
		schedule.NewService(schedule.NewStore(db, sessions), sessions),
		cfg,
		mcpstore.NewStore(db),
		configapply.New(
			configops.New(filepath.Join(home, "config.yaml"), filepath.Join(home, "secrets.yaml")),
			sessions,
		),
	)

	t.Cleanup(func() { service.Shutdown(context.Background(), 3*time.Second) })
	controllers := managercontrol.New(
		service,
		sessions,
		managerdiscovery.New(sessions, cfg, nil),
		bus,
		cfg,
		nil,
	)

	return &delayedTelegramHarness{
		controller: controllers.ForManager(delayedTelegramManagerID), controllers: controllers, sessions: sessions,
		service: service, workDir: workDir, recorder: &delayedTelegramRecorder{},
	}
}

func TestHarnessScenario_StartFailureCannotFloodTelegram(t *testing.T) {
	var attempts atomic.Int64
	h := newDelayedTelegramHarnessWithClient(t, func(*config.Config) (llm.Client, error) {
		attempts.Add(1)
		return nil, errors.New("model removed-model not found in config")
	})
	id := h.produceBeforeManager(t)
	require.Eventually(t, func() bool {
		record, err := h.sessions.GetSession(t.Context(), id)
		return err == nil && record.Status == sessionstore.SessionStatusError && !h.service.HasActiveLoop(id)
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1), attempts.Load())
	for range 1109 {
		reported, err := h.sessions.RecordSessionStartFailure(t.Context(), id, "same failed work observed again")
		require.NoError(t, err)
		assert.False(t, reported)
	}
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), id))
	for round := range 2 {
		manager, err := New(config.ManagerEntry{
			ID: delayedTelegramManagerID, BotToken: "test-token", TargetChatID: new(int64(harnessChatID)),
		}, &config.UnifiedConfig{}, h.controller)
		require.NoError(t, err)
		manager.httpClient = &http.Client{Transport: h.recorder}
		require.NoError(t, manager.Start(t.Context()))
		awaitChatTail(t, h, id)
		require.NoError(t, manager.Stop(context.Background()))
		count := 0
		for _, call := range h.recorder.snapshot() {
			if call.Method == "sendMessage" && strings.Contains(call.Text, "Session error") {
				count++
			}
		}
		assert.Equal(t, 1, count, "manager activation %d", round)
	}
	assert.Equal(t, int64(1), attempts.Load())
	pending, err := h.sessions.PeekPending(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "finish before telegram starts", pending.RawContent)
}

func (h *delayedTelegramHarness) produceBeforeManager(t *testing.T) int64 {
	t.Helper()
	sessionID, err := h.controller.CreateSession(t.Context(), controllerapi.SessionCreateData{
		WorkDir: h.workDir, Prompt: "finish before telegram starts", Model: "fake-model",
	})
	require.NoError(t, err)

	return sessionID
}

func (h *delayedTelegramHarness) waitForBacklog(t *testing.T, sessionID int64) {
	t.Helper()
	chat := h.controller.(controllerapi.ChatFocusController)
	require.Eventually(t, func() bool {
		rows, err := chat.ChatOutputs(t.Context(), strconv.FormatInt(harnessChatID, 10), sessionID, 0, 100)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.Content == "delayed telegram answer" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond)
}

func awaitChatTail(t *testing.T, h *delayedTelegramHarness, sessionID int64) {
	t.Helper()
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.Eventually(t, func() bool {
		focus, cursor, entered, err := chat.ChatCursor(t.Context(), key)
		if err != nil || focus != sessionID || !entered {
			return false
		}
		rows, err := chat.ChatOutputs(t.Context(), key, sessionID, cursor, 100)
		return err == nil && len(rows) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

type delayedTelegramClient struct{}

func (delayedTelegramClient) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return &llmwire.Response{Text: "delayed telegram answer", FinishType: llmwire.FinishStop}, nil
}

func (delayedTelegramClient) Model() string                          { return "fake-model" }
func (delayedTelegramClient) APIKey() string                         { return "" }
func (delayedTelegramClient) Close() error                           { return nil }
func (delayedTelegramClient) Provider() string                       { return "fake" }
func (delayedTelegramClient) ContextWindow() int                     { return 200000 }
func (delayedTelegramClient) SetReasoningLevel(string)               {}
func (delayedTelegramClient) SetImageAuthorizer(llm.ImageAuthorizer) {}
func (delayedTelegramClient) GetReasoningLevel() string              { return "medium" }
func (delayedTelegramClient) SetSessionID(string)                    {}

type delayedTelegramCall struct {
	Method    string
	Text      string
	ChatID    int64
	ParseMode string
}

type delayedTelegramRecorder struct {
	mu            sync.Mutex
	calls         []delayedTelegramCall
	sendResponses []string
}

func (r *delayedTelegramRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	method := filepath.Base(request.URL.Path)
	if method == "getUpdates" {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}

	if !slices.Contains([]string{"getMe", "getChat", "setMyCommands", "sendMessage", "sendChatAction"}, method) {
		return nil, fmt.Errorf("unexpected Telegram method %s", method)
	}

	var payload struct {
		Text        string          `json:"text"`
		ChatID      int64           `json:"chat_id"`
		ParseMode   string          `json:"parse_mode"`
		Commands    json.RawMessage `json:"commands"`
		LinkPreview json.RawMessage `json:"link_preview_options"`
		Action      string          `json:"action"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s request: %w", method, err)
	}

	r.mu.Lock()
	r.calls = append(r.calls, delayedTelegramCall{
		Method: method, Text: payload.Text, ChatID: payload.ChatID, ParseMode: payload.ParseMode,
	})
	response := ""
	if method == "sendMessage" && len(r.sendResponses) > 0 {
		response, r.sendResponses = r.sendResponses[0], r.sendResponses[1:]
	}
	r.mu.Unlock()

	if response != "" {
		return harnessResponse(request, response), nil
	}

	result := "true"
	switch method {
	case "getMe":
		result = `{"id":42}`
	case "getChat":
		result = fmt.Sprintf(`{"id":%d,"type":"private"}`, harnessChatID)
	case "sendMessage":
		if strings.TrimSpace(payload.Text) == "" {
			return harnessResponse(request, `{"ok":false,"error_code":400,"description":"message text is empty"}`), nil
		}
		result = `{"message_id":123}`
	}
	return harnessResponse(request, `{"ok":true,"result":`+result+"}"), nil
}

func (r *delayedTelegramRecorder) snapshot() []delayedTelegramCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]delayedTelegramCall(nil), r.calls...)
}

func hasTelegramMessage(calls []delayedTelegramCall, text string) bool {
	for _, call := range calls {
		if call.Method == "sendMessage" && call.Text == text {
			return true
		}
	}

	return false
}

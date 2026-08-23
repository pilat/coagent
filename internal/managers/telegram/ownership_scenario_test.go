package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type telegramOwnershipHarness struct {
	svc           daemon.Service
	sessions      sessionstore.Store
	projectID     int64
	cliController controllerapi.Controller
	recorder      *ownershipTelegramRecorder
}

// A CLI-owned session must not create a topic or send a message through this manager.
func TestHarnessScenario_DurableManagerOwnershipReachesOnlyTelegramRenderer(t *testing.T) {
	h := newTelegramOwnershipHarness(t)
	owned, foreign := h.createSessions(t)
	cliEvents := h.cliController.Subscribe()
	t.Cleanup(func() { h.cliController.Unsubscribe(cliEvents) })

	h.publish(owned, createdTelegramSession())
	h.publish(owned, message("✅ telegram owner answer"))
	h.publish(foreign, createdCLISession())
	h.publish(foreign, message("❌ local chat answer"))
	h.publish(owned, message("✅ telegram owner barrier"))

	assertTelegramCalls(t, h.recorder)
	assertCLIControllerEvents(t, cliEvents, foreign)
}

func newTelegramOwnershipHarness(t *testing.T) *telegramOwnershipHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "coagent.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	projects := daemon.NewStore(db)
	sessions := sessionstore.NewStore(db)
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc := daemon.New(nil, projects, sessions, sessions, daemon.NewLinkStore(db), nil, cfg, nil, nil, nil)
	controllers := daemon.NewController(svc, cfg, nil, nil)
	telegramController := controllers.ForManager("telegram-main")
	projectID, err := projects.GetOrCreateProject(ctx, filepath.Join(root, "project"))
	require.NoError(t, err)

	recorder := &ownershipTelegramRecorder{}
	manager, err := New(config.ManagerEntry{
		ID: "telegram-main", BotToken: "test-token", TargetChatID: targetID(harnessChatID),
	}, cfg.UnifiedConfig, telegramController)
	require.NoError(t, err)
	manager.httpClient = &http.Client{Transport: recorder}
	manager.subscription = telegramController.Subscribe()

	loopCtx, cancel := context.WithCancel(ctx)
	var loop sync.WaitGroup
	loop.Go(func() { manager.notificationsLoop(loopCtx) })
	t.Cleanup(func() {
		cancel()
		telegramController.Unsubscribe(manager.subscription)
		loop.Wait()
	})

	return &telegramOwnershipHarness{
		svc: svc, sessions: sessions, projectID: projectID, recorder: recorder,
		cliController: controllers.ForManager(controllerapi.BuiltinCLIManagerID),
	}
}

func (h *telegramOwnershipHarness) createSessions(t *testing.T) (int64, int64) {
	t.Helper()
	owned, err := h.sessions.CreateSession(context.Background(), h.projectID, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)
	foreign, err := h.sessions.CreateSession(context.Background(), h.projectID, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: controllerapi.BuiltinCLIManagerID,
	})
	require.NoError(t, err)

	return owned.ID, foreign.ID
}

func (h *telegramOwnershipHarness) publish(sessionID int64, n sessionevent.Notification) {
	h.svc.NotifySession(sessionID, n)
}

func createdTelegramSession() sessionevent.Notification {
	return createdSession("project - telegram", "telegram-main")
}

func createdCLISession() sessionevent.Notification {
	return createdSession("project - cli", controllerapi.BuiltinCLIManagerID)
}

func createdSession(name, managerID string) sessionevent.Notification {
	return sessionevent.Notification{
		Type: sessionevent.NotifySessionCreated, Name: name, WorkDir: filepath.Join("/tmp", "project"),
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: managerID},
	}
}

func message(text string) sessionevent.Notification {
	return sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: text}
}

func assertTelegramCalls(t *testing.T, recorder *ownershipTelegramRecorder) {
	t.Helper()
	require.Eventually(t, func() bool {
		calls := recorder.snapshot()
		return len(calls) >= 3 && calls[len(calls)-1].Text == "✅ telegram owner barrier"
	}, time.Second, 10*time.Millisecond, "owned barrier must be rendered after every prior notification")
	assert.Equal(t, []telegramHarnessCall{
		{Method: "createForumTopic", ChatID: harnessChatID},
		{
			Method:    "sendMessage",
			ChatID:    harnessChatID,
			ThreadID:  harnessTopicID,
			Text:      "✅ telegram owner answer",
			ParseMode: tgParseModeHTML,
		},
		{
			Method:    "sendMessage",
			ChatID:    harnessChatID,
			ThreadID:  harnessTopicID,
			Text:      "✅ telegram owner barrier",
			ParseMode: tgParseModeHTML,
		},
	}, recorder.snapshot())
}

func assertCLIControllerEvents(t *testing.T, events <-chan controllerapi.SessionNotification, sessionID int64) {
	t.Helper()
	for _, want := range []sessionevent.NotificationType{sessionevent.NotifySessionCreated, sessionevent.NotifyMessage} {
		select {
		case got := <-events:
			assert.Equal(t, sessionID, got.SessionID)
			assert.Equal(t, want, got.Notification.Type)
		case <-time.After(3 * time.Second):
			t.Fatalf("CLI manager did not receive its %s notification", want)
		}
	}
}

type ownershipTelegramRecorder struct {
	mu    sync.Mutex
	calls []telegramHarnessCall
}

func (r *ownershipTelegramRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	parts := strings.Split(request.URL.Path, "/")
	method := parts[len(parts)-1]
	var payload struct {
		ChatID    int64  `json:"chat_id"`
		ThreadID  int64  `json:"message_thread_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.calls = append(r.calls, telegramHarnessCall{
		Method: method, ChatID: payload.ChatID, ThreadID: payload.ThreadID,
		Text: payload.Text, ParseMode: payload.ParseMode,
	})
	r.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`{"ok":true,"result":{"message_id":123,"message_thread_id":7001}}`,
		)),
		Request: request,
	}, nil
}

func (r *ownershipTelegramRecorder) snapshot() []telegramHarnessCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]telegramHarnessCall(nil), r.calls...)
}

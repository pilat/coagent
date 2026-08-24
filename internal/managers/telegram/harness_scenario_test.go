package telegram

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

// The inputs of this scenario are not authored here: internal/testdata/harness_scenarios
// holds the exact notification traces the daemon scenarios recorded from a real
// run. Only the rendered Telegram traffic below is this package's own golden.
const (
	harnessSessionID = 42
	harnessChatID    = -100123
	harnessTopicID   = 7001
	renderedGolden   = "rendered.json"
)

// updateHarnessTraces rewrites the rendered golden instead of asserting it:
// go test ./internal/managers/telegram -run TestHarnessScenario -update-traces
var updateHarnessTraces = flag.Bool(
	"update-traces", false, "rewrite the rendered telegram golden under testdata",
)

// harnessTraceFile mirrors the shared schema written by internal/daemon. Unknown
// fields are rejected so a schema change on the daemon side cannot pass silently.
type harnessTraceFile struct {
	SourceTest string              `json:"source_test"`
	Trace      []harnessTraceEvent `json:"trace"`
}

type harnessTraceEvent struct {
	Type       string             `json:"type"`
	Message    string             `json:"message,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Source     string             `json:"source,omitempty"`
	Name       string             `json:"name,omitempty"`
	WorkDir    string             `json:"work_dir,omitempty"`
	Attributes map[string]any     `json:"attributes,omitempty"`
	Waiting    []harnessTraceWait `json:"waiting,omitempty"`
}

type harnessTraceWait struct {
	Kind  string `json:"kind"`
	Child string `json:"child,omitempty"`
	Wake  string `json:"wake,omitempty"`
}

type telegramRenderedFile struct {
	Scenarios []telegramRenderedScenario `json:"scenarios"`
}

type telegramRenderedScenario struct {
	Trace      string                `json:"trace"`
	SourceTest string                `json:"source_test"`
	Calls      []telegramHarnessCall `json:"calls"`
}

type telegramHarnessCall struct {
	Method    string `json:"method"`
	ChatID    int64  `json:"chat_id"`
	ThreadID  int64  `json:"thread_id,omitempty"`
	Text      string `json:"text,omitempty"`
	ParseMode string `json:"parse_mode,omitempty"`
}

func TestHarnessScenario_DaemonTraceRendersExactlyOnceInTelegram(t *testing.T) {
	names := daemonTraceNames(t)

	got := telegramRenderedFile{}
	for _, name := range names {
		trace := readDaemonTrace(t, name)
		got.Scenarios = append(got.Scenarios, telegramRenderedScenario{
			Trace:      name,
			SourceTest: trace.SourceTest,
			Calls:      replayDaemonTrace(t, trace),
		})
	}

	if *updateHarnessTraces {
		writeRenderedGolden(t, got)

		return
	}

	want := readRenderedGolden(t)
	require.Len(t, got.Scenarios, len(want.Scenarios),
		"every recorded daemon trace must have a rendered golden; regenerate with -update-traces")

	for i, scenario := range got.Scenarios {
		t.Run(scenario.Trace, func(t *testing.T) {
			assert.Equal(t, want.Scenarios[i], scenario, "Telegram traffic rendered from the daemon trace")
		})
	}
}

// This is the exact reported cross-manager leak: a local CLI conversation is
// visible on the global notification subscription, but it must produce no
// Telegram traffic — neither a topic nor the model's answer.
func TestHarnessScenario_CLIConversationIsNotRenderedInTelegram(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)

	manager.handleNotification(t.Context(), controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type:       sessionevent.NotifySessionCreated,
			Name:       "sys:coagent - 42",
			WorkDir:    "/tmp/projects/sys_coagent",
			Attributes: map[string]any{"manager_id": "cli", "channel": "cli"},
		},
	})
	manager.handleNotification(t.Context(), controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type:    sessionevent.NotifyMessage,
			Message: "✅ configuration answer",
		},
	})

	assert.Empty(t, calls, "a Telegram manager must ignore another manager's conversation")
}

func TestHarnessScenario_BotForumGeneralGuidesWithoutCreatingSession(t *testing.T) {
	var calls []telegramHarnessCall
	controller := &fakeController{}
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.cfg.AllowedUserIDs = []int64{7}
	manager.cfg.TargetChatID = nil
	manager.cfg.ServiceTopicName = "Coagent"
	manager.target = forumTarget{chatID: 7, topology: forumTopologyBot}

	manager.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: 7}, Text: "hello",
	}})

	require.Len(t, calls, 1)
	assert.Equal(t, telegramHarnessCall{
		Method: "sendMessage", ChatID: 7,
		Text: "Open the “Coagent” topic to create or manage sessions.", ParseMode: tgParseModeHTML,
	}, calls[0])
	assert.Empty(t, controller.createSessionCalls)
}

func TestHarnessScenario_TelegramManagersRenderOnlyTheirOwnConversation(t *testing.T) {
	var primaryCalls, secondaryCalls []telegramHarnessCall
	primary := newTelegramHarnessManager(t, &fakeController{}, &primaryCalls)
	secondary := newTelegramHarnessManager(t, &fakeController{}, &secondaryCalls)
	secondary.id = "telegram-secondary"
	secondary.cfg.ID = "telegram-secondary"

	created := controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type:       sessionevent.NotifySessionCreated,
			Name:       "project - 42",
			WorkDir:    "/tmp/project",
			Attributes: map[string]any{controllerapi.SessionAttributeManagerID: "telegram-main"},
		},
	}
	answer := controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: "✅ private answer",
		},
	}

	primary.handleNotification(t.Context(), created)
	secondary.handleNotification(t.Context(), created)
	primary.handleNotification(t.Context(), answer)
	secondary.handleNotification(t.Context(), answer)

	require.Len(t, primaryCalls, 2)
	assert.Equal(t, "createForumTopic", primaryCalls[0].Method)
	assert.Equal(t, "✅ private answer", primaryCalls[1].Text)
	assert.Empty(t, secondaryCalls)
}

// CLI-owned config-apply traces must produce no Telegram traffic.
// This non-golden guard prevents regenerating a hidden ownership regression.
func TestHarnessScenario_SetManagerTracesProduceNoTelegramTraffic(t *testing.T) {
	for _, name := range []string{
		"set_manager_patch_restart.json",
		"set_manager_reapply_restart.json",
	} {
		t.Run(name, func(t *testing.T) {
			trace := readDaemonTrace(t, name)
			calls := replayDaemonTrace(t, trace)
			assert.Empty(t, calls, "a CLI-owned trace must produce no Telegram traffic")
		})
	}
}

// replayDaemonTrace feeds one recorded trace through the production notification
// handler and returns the resulting Telegram API traffic.
func replayDaemonTrace(t *testing.T, trace harnessTraceFile) []telegramHarnessCall {
	t.Helper()

	var calls []telegramHarnessCall
	attributes := map[int64]map[string]any{
		harnessSessionID: {controllerapi.SessionAttributeManagerID: "telegram-main"},
	}
	controller := &fakeController{setAttrsHook: func(data controllerapi.SessionSetAttributesData) {
		attributes[data.SessionID] = roundTripAttributes(t, data.Attributes)
	}}
	manager := newTelegramHarnessManager(t, controller, &calls)

	for _, event := range trace.Trace {
		n := harnessNotification(t, event)
		if n.Type == sessionevent.NotifySessionCreated && len(n.Attributes) > 0 {
			attrs := maps.Clone(attributes[harnessSessionID])
			maps.Copy(attrs, n.Attributes)
			attributes[harnessSessionID] = roundTripAttributes(t, attrs)
		}
		if n.Type == sessionevent.NotifySessionCreated || n.Type == sessionevent.NotifySessionCleared {
			n.Attributes = attributes[harnessSessionID]
		}
		require.NoError(t, n.Validate(), "recorded daemon notification must satisfy the union contract")
		manager.handleNotification(t.Context(), controllerapi.SessionNotification{
			SessionID: harnessSessionID, Notification: n,
		})
	}

	return calls
}

// harnessNotification rebuilds a notification from its recorded form. Ids and wake
// times were normalized on the daemon side; only their shape has to survive.
func harnessNotification(t *testing.T, event harnessTraceEvent) sessionevent.Notification {
	t.Helper()

	n := sessionevent.Notification{
		Type:       sessionevent.NotificationType(event.Type),
		Message:    event.Message,
		Name:       event.Name,
		Status:     sessionevent.State(event.Status),
		Reason:     event.Reason,
		Source:     event.Source,
		WorkDir:    event.WorkDir,
		Attributes: event.Attributes,
	}

	wake := time.Date(2026, time.January, 2, 15, 4, 0, 0, time.UTC)
	for i, item := range event.Waiting {
		wait := sessionevent.WaitItem{Kind: sessionevent.WaitKind(item.Kind)}
		switch wait.Kind {
		case sessionevent.WaitSubagent:
			wait.ChildID = int64(i + 1)
		case sessionevent.WaitSleep:
			wait.WakeAt = &wake
		}
		n.Waiting = append(n.Waiting, wait)
	}

	return n
}

// roundTripAttributes mimics the daemon storing attributes as JSON and echoing
// them back on the next announcement: numbers come back as float64.
func roundTripAttributes(t *testing.T, attrs map[string]any) map[string]any {
	t.Helper()

	data, err := json.Marshal(attrs)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))

	return out
}

func daemonTraceNames(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(daemonTraceDir(), "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no recorded daemon traces found")

	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}

	return names
}

// daemonTraceDir is the shared fixture root internal/daemon records into: both
// halves of the compositional proof read the same bytes.
func daemonTraceDir() string {
	return filepath.Join("..", "..", "testdata", "harness_scenarios")
}

func readDaemonTrace(t *testing.T, name string) harnessTraceFile {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(daemonTraceDir(), name))
	require.NoError(t, err)

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var file harnessTraceFile
	require.NoError(t, decoder.Decode(&file))
	require.NotEmpty(t, file.Trace)

	return file
}

func readRenderedGolden(t *testing.T) telegramRenderedFile {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "harness_scenarios", renderedGolden))
	require.NoError(t, err, "missing rendered golden; regenerate with -update-traces")

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var file telegramRenderedFile
	require.NoError(t, decoder.Decode(&file))

	return file
}

func writeRenderedGolden(t *testing.T, file telegramRenderedFile) {
	t.Helper()

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(file))

	path := filepath.Join("testdata", "harness_scenarios", renderedGolden)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	t.Logf("recorded rendered telegram golden %s", path)
}

func newTelegramHarnessManager(
	t *testing.T,
	controller *fakeController,
	calls *[]telegramHarnessCall,
) *Manager {
	t.Helper()

	// No topic is pre-registered: the recorded session_created event is what must
	// bind one, exactly as it does in production.
	return &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID: "telegram-main", BotToken: "test-token", TargetChatID: targetID(harnessChatID),
			SendChunkDelayMS: 0,
		},
		controller:     controller,
		httpClient:     &http.Client{Transport: telegramHarnessRecorder(t, calls)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}
}

func telegramHarnessRecorder(t *testing.T, calls *[]telegramHarnessCall) roundTripFunc {
	t.Helper()

	return func(request *http.Request) (*http.Response, error) {
		var body struct {
			ChatID    int64  `json:"chat_id"`
			ThreadID  int64  `json:"message_thread_id"`
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		parts := strings.Split(request.URL.Path, "/")
		*calls = append(*calls, telegramHarnessCall{
			Method: parts[len(parts)-1], ChatID: body.ChatID, ThreadID: body.ThreadID,
			Text: body.Text, ParseMode: body.ParseMode,
		})

		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"ok":true,"result":{"message_id":123,"message_thread_id":` +
					strconv.Itoa(harnessTopicID) + `}}`,
			)),
			Header:  make(http.Header),
			Request: request,
		}, nil
	}
}

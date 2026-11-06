package telegram

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

const (
	harnessSessionID = 42
	harnessChatID    = 7
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
	SourceTest string               `json:"source_test"`
	Trace      []harnessTraceEvent  `json:"trace"`
	Outputs    []harnessTraceOutput `json:"outputs"`
	Owners     map[int64]string     `json:"owners,omitempty"`
}

type harnessTraceOutput struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	SourceKey string `json:"source_key,omitempty"`
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
	Text      string `json:"text,omitempty"`
	ParseMode string `json:"parse_mode,omitempty"`
}

func TestHarnessScenario_ForeignConversationIsNotRenderedInTelegram(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)

	manager.handleNotification(t.Context(), controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type:       sessionevent.NotifySessionCreated,
			Name:       "other - 42",
			WorkDir:    "/tmp/projects/other",
			Attributes: map[string]any{"manager_id": "telegram-other"},
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
	manager, err := New(config.ManagerEntry{
		ID: "telegram-main", BotToken: "test-token", AllowedUserIDs: []int64{harnessChatID},
	}, nil, controller)
	require.NoError(t, err)
	manager.httpClient = &http.Client{Transport: telegramHarnessRecorder(t, calls)}
	return manager
}

func telegramHarnessRecorder(t *testing.T, calls *[]telegramHarnessCall) roundTripFunc {
	t.Helper()
	return func(request *http.Request) (*http.Response, error) {
		var body struct {
			ChatID    int64  `json:"chat_id"`
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		*calls = append(*calls, telegramHarnessCall{
			Method: filepath.Base(request.URL.Path), ChatID: body.ChatID, Text: body.Text, ParseMode: body.ParseMode,
		})
		return telegramResponse(request, `{"ok":true,"result":{"message_id":123}}`), nil
	}
}

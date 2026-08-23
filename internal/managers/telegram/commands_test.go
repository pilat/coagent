package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestNormalizeTextCommand(t *testing.T) {
	assert.Equal(t, "/spawn", normalizeTextCommand("/spawn@mybot"))
	assert.Equal(t, "/spawn now", normalizeTextCommand("/spawn@mybot now"))
	assert.Equal(t, "hello", normalizeTextCommand(" hello "))
}

func TestHandleCommandsPreservesCanonicalSkillNames(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Manager)
		want []string
	}{
		{
			name: "commands",
			run: func(m *Manager) {
				m.handleCommands(context.Background(), 42, 99)
			},
			want: []string{"<b>Skills:</b>\n  /skill pilat:plan-handoff&lt;&amp;&gt; — Use &lt;plan&gt; &amp; ship"},
		},
		{
			name: "session help",
			run: func(m *Manager) {
				m.handleHelp(context.Background(), 42, 99)
			},
			want: []string{
				"<b>Commands:</b>\n" +
					"  /new — new dialog project by name (/new &lt;name&gt;), or bare /new to pick one\n" +
					"  /spawn — open folder picker for new session\n" +
					"  /kill — end this session (terminal)\n" +
					"  /stop — stop the current run (session stays, resumable)\n" +
					"  /clear — clear session (fresh start, same topic)\n" +
					"  /compact — compact context now; /compact &lt;focus&gt; to steer the summary\n" +
					"  /model — choose LLM model\n" +
					"  /status — show session stats (tokens, cost, context)\n" +
					"  /schedules — list this session's schedules (ask me to add/change them)\n" +
					"  /help — this message",
				"<b>Skills:</b>\n  /skill pilat:plan-handoff&lt;&amp;&gt; — Use &lt;plan&gt; &amp; ship",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := make([]string, 0, len(tc.want))
			m := &Manager{
				cfg: config.ManagerEntry{
					BotToken:     "token",
					TargetChatID: targetID(-100123),
				},
				controller: &fakeController{
					listSkills: []controllerapi.ConfigSkillInfo{{
						Name:        "pilat:plan-handoff<&>",
						Description: "Use <plan> & ship",
					}},
				},
				httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
			}

			tc.run(m)

			assert.Equal(t, tc.want, messages)
		})
	}
}

func TestSchedulesCommandBypassesModel(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/schedules")

	// Read-only command: it queries the session's schedules and never steers the
	// model (the agent must not see the command).
	require.Len(t, ctrl.scheduleCalls, 1)
	assert.Equal(t, int64(42), ctrl.scheduleCalls[0].SessionID)
	assert.Empty(t, ctrl.messageCalls)
}

func TestStopCommandStopsWithoutSteering(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
	}

	m.handleSessionTopicMessage(context.Background(), 42, 99, "/stop")

	// /stop halts the current run via the controller; it must never be forwarded to
	// the model as a steering message.
	require.Equal(t, []int64{42}, ctrl.stopCalls)
	assert.Empty(t, ctrl.messageCalls)
}

func TestHandleSchedulesFormatsEntries(t *testing.T) {
	var messages []string
	firedAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	ctrl := &fakeController{
		listSchedules: []controllerapi.ScheduleInfo{
			{
				ID:          12,
				Cron:        "0 9 * * *",
				Timezone:    "Europe/Berlin",
				Fresh:       true,
				Prompt:      "check CI",
				LastFiredAt: &firedAt,
			},
			{ID: 13, OneShotAt: &firedAt, Prompt: "wake once"},
		},
	}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
	}

	m.handleSchedules(context.Background(), 42, 99)

	require.Len(t, messages, 1)
	out := messages[0]
	assert.Contains(t, out, "Schedules (2)")
	assert.Contains(t, out, "#12")
	assert.Contains(t, out, "0 9 * * *")
	assert.Contains(t, out, "Europe/Berlin")
	assert.Contains(t, out, "🆕 fresh")
	assert.Contains(t, out, "check CI")
	assert.Contains(t, out, "#13")
	assert.Contains(t, out, "once 2026-07-21 09:00 UTC")
}

func TestHandleSchedulesEmpty(t *testing.T) {
	var messages []string
	ctrl := &fakeController{}
	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		controller: ctrl,
		httpClient: &http.Client{Transport: telegramMessageRecorder(t, &messages)},
	}

	m.handleSchedules(context.Background(), 42, 99)

	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "No schedules")
}

func TestHandleSessionTopicMessagePreservesCanonicalText(t *testing.T) {
	ctrl := &fakeController{}
	m := &Manager{controller: ctrl}
	text := "/pilat:plan-handoff /pilat___plan__handoff"

	m.handleSessionTopicMessage(context.Background(), 42, 99, text)

	require.Len(t, ctrl.messageCalls, 1)
	assert.Equal(t, controllerapi.SessionMessageData{SessionID: 42, Message: text}, ctrl.messageCalls[0])
}

func TestParseCallbackDataMatrix(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		want   callbackAction
		wantOK bool
	}{
		{name: "nav", data: "nav:7", want: callbackAction{Kind: callbackNav, DirID: 7}, wantOK: true},
		{name: "launch", data: "launch:9", want: callbackAction{Kind: callbackLaunch, DirID: 9}, wantOK: true},
		{
			name:   "launch_gwt",
			data:   "launch_gwt:10",
			want:   callbackAction{Kind: callbackLaunchGWT, DirID: 10},
			wantOK: true,
		},
		{
			name:   "more",
			data:   "more:11:20",
			want:   callbackAction{Kind: callbackMore, DirID: 11, Offset: 20},
			wantOK: true,
		},
		{name: "spawn", data: "spawn", want: callbackAction{Kind: callbackSpawn}, wantOK: true},
		{
			name:   "newpick",
			data:   "newpick:7",
			want:   callbackAction{Kind: callbackNewPick, ProjectID: 7},
			wantOK: true,
		},
		{
			name:   "newpage",
			data:   "newpage:20",
			want:   callbackAction{Kind: callbackNewPage, Offset: 20},
			wantOK: true,
		},
		{name: "newpick_bad", data: "newpick:x", wantOK: false},
		{name: "kill", data: "kill:42", want: callbackAction{Kind: callbackKill, Session: 42}, wantOK: true},
		{
			name:   "model",
			data:   "model:local:gpt-5",
			want:   callbackAction{Kind: callbackModel, ModelID: "local:gpt-5"},
			wantOK: true,
		},
		{name: "invalid_more", data: "more:bad", wantOK: false},
		{name: "unknown", data: "noop", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCallbackData(tc.data)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.want.Kind, got.Kind)
			assert.Equal(t, tc.want.DirID, got.DirID)
			assert.Equal(t, tc.want.Offset, got.Offset)
			assert.Equal(t, tc.want.Session, got.Session)
			assert.Equal(t, tc.want.ModelID, got.ModelID)
			assert.Equal(t, tc.want.ProjectID, got.ProjectID)
		})
	}
}

func TestDirPageBounds(t *testing.T) {
	start, end, hasMore := dirPageBounds(25, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 10, end)
	assert.True(t, hasMore)

	start, end, hasMore = dirPageBounds(25, 20)
	assert.Equal(t, 20, start)
	assert.Equal(t, 25, end)
	assert.False(t, hasMore)

	start, end, hasMore = dirPageBounds(25, 999)
	assert.Equal(t, 0, start)
	assert.Equal(t, 10, end)
	assert.True(t, hasMore)

	start, end, hasMore = dirPageBounds(0, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 0, end)
	assert.False(t, hasMore)

	require.NotPanics(t, func() {
		_, _, _ = dirPageBounds(10, -5)
	})
}

func telegramMessageRecorder(t *testing.T, messages *[]string) roundTripFunc {
	t.Helper()

	return func(req *http.Request) (*http.Response, error) {
		var body struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		*messages = append(*messages, body.Text)

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":123}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
}

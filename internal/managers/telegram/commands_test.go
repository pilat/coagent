package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestNormalizeTextCommand(t *testing.T) {
	assert.Equal(t, "/spawn", normalizeTextCommand("/spawn@mybot"))
	assert.Equal(t, "/spawn now", normalizeTextCommand("/spawn@mybot now"))
	assert.Equal(t, "hello", normalizeTextCommand(" hello "))
}

func TestHandleCommandsPreservesCanonicalSkillNames(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, listSkills: []controllerapi.ConfigSkillInfo{{
		Name: "pilat:plan-handoff<&>", Description: "Use <plan> & ship",
	}}}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/help")
	require.Len(t, messages, 2)
	assert.Contains(t, messages[0], "/switch")
	assert.Contains(t, messages[0], "/title")
	assert.Equal(
		t,
		"<b>Skills:</b>\n  /skill pilat:plan-handoff&lt;&amp;&gt; — Use &lt;plan&gt; &amp; ship",
		messages[1],
	)
	m.handleChatText(t.Context(), "/start")
	assert.Equal(t, messages[:2], messages[2:])
}

func TestFocusedCommandsEnterDurableInboxVerbatim(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42}
	m := newCmdTestManager(t, ctrl, &messages)
	inputs := []string{
		"/schedules",
		"/stop",
		"/clear",
		"/kill",
		"/compact keep tests",
		"/status",
		"/budget 1 usd",
		"/skill pilat:plan-handoff",
		"/pilat:plan-handoff /pilat___plan__handoff",
	}
	for _, text := range inputs {
		m.handleChatText(t.Context(), text)
	}
	require.Len(t, ctrl.messageCalls, len(inputs))
	for i, text := range inputs {
		assert.Equal(t, controllerapi.SessionMessageData{SessionID: 42, Message: text}, ctrl.messageCalls[i])
	}
	assert.Empty(t, messages)
	m.handleChatText(t.Context(), "/config")
	require.Len(t, ctrl.messageCalls, len(inputs)+1)
	assert.Equal(t, controllerapi.SessionMessageData{SessionID: 42, Message: "/config"}, ctrl.messageCalls[len(inputs)])
	assert.Empty(t, messages)
}

func TestNumberedControlsDoNotChangeSelection(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, listSessions: []controllerapi.SessionInfo{
		{ID: 42, Name: "selected", Attributes: map[string]any{"manager_id": "telegram-main"}},
		{ID: 9, Name: "other", Attributes: map[string]any{"manager_id": "telegram-main"}},
		{ID: 8, Name: "foreign", Attributes: map[string]any{"manager_id": "other"}},
	}}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/kill 9")
	m.handleChatText(t.Context(), "/stop 9")
	assert.Equal(
		t,
		[]controllerapi.SessionMessageData{{SessionID: 9, Message: "/kill"}, {SessionID: 9, Message: "/stop"}},
		ctrl.messageCalls,
	)
	assert.Equal(t, int64(42), ctrl.focus)
	for _, text := range []string{"/kill 8", "/stop 0", "/kill absent"} {
		m.handleChatText(t.Context(), text)
	}
	assert.Len(t, ctrl.messageCalls, 2)
	assert.Len(t, messages, 3)
	assert.Equal(t, int64(42), ctrl.focus)
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

package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestChatOpen_OnAFreshDaemonHasNothingToResume(t *testing.T) {
	h := newHarness(t)

	res := openChat(t, h.dial(t))
	assert.Equal(t, int64(0), res.SessionID, "the first message is what starts the conversation")
	assert.Equal(t, "/projects/sys_coagent", res.WorkDir)
	assert.Equal(t, []controllerapi.ProjectCreateData{{
		Name: controllerapi.CoagentSystemProjectName, System: true,
	}}, h.ctrl.projects)
}

func TestChatOpen_ClaimsALegacyCLIConversationForScopedDelivery(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{
		ID: 77, ProjectID: chatProjectID, UpdatedAt: time.Now(),
		Attributes: map[string]any{"channel": "cli"},
	}}

	res := openChat(t, h.dial(t))

	assert.Equal(t, int64(77), res.SessionID)
	require.Len(t, h.ctrl.setAttrs, 1)
	assert.Equal(t, int64(77), h.ctrl.setAttrs[0].SessionID)
	assert.Equal(t, "cli", h.ctrl.setAttrs[0].Attributes[controllerapi.SessionAttributeManagerID])
}

func TestChatOpen_IgnoresAConversationOwnedByAnotherManager(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{
		ID: 77, ProjectID: chatProjectID, UpdatedAt: time.Now(),
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "telegram-main",
			"channel":                               "telegram",
		},
	}}

	res := openChat(t, h.dial(t))

	assert.Zero(t, res.SessionID)
	assert.Empty(t, h.ctrl.setAttrs)
}

func TestChatSend_FirstMessageCreatesTheSessionOnTheCLIChannel(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t)

	openChat(t, c)

	res := sendChat(t, c, SendParams{Text: "hello"})
	assert.NotZero(t, res.SessionID)

	require.Len(t, h.ctrl.created, 1)
	assert.Equal(t, "hello", h.ctrl.created[0].Prompt)
	assert.Equal(t, "claude-sonnet-5", h.ctrl.created[0].Model)
	assert.Equal(t, map[string]any{"channel": "cli"}, h.ctrl.created[0].Attributes)
	assert.Equal(t, "/projects/sys_coagent", h.ctrl.created[0].WorkDir)
	assert.Equal(t, controllerapi.CoagentSystemProjectName, h.ctrl.created[0].SystemProject)
}

func TestChatSend_FirstMessageUsesModelSelectedBeforeSessionExists(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t)

	openChat(t, c)

	res := sendChat(t, c, SendParams{Text: "hello", Model: "openai/gpt-5"})
	assert.NotZero(t, res.SessionID)

	require.Len(t, h.ctrl.created, 1)
	assert.Equal(t, "openai/gpt-5", h.ctrl.created[0].Model)
}

func TestChatSend_AppliesPendingModelWhenAnotherTerminalCreatedTheSession(t *testing.T) {
	h := newHarness(t)
	first, second := h.dial(t), h.dial(t)

	openChat(t, first)
	openChat(t, second)
	created := sendChat(t, second, SendParams{Text: "start on the default"})
	sendChat(t, first, SendParams{Text: "continue", Model: "openai/gpt-5"})

	assert.Equal(t, []controllerapi.SessionSetModelData{{
		SessionID: created.SessionID, Model: "openai/gpt-5",
	}}, h.ctrl.setModel)
	require.Len(t, h.ctrl.sent, 1)
	assert.Equal(t, created.SessionID, h.ctrl.sent[0].SessionID)
}

func TestChatModels_ReturnsCatalogAndCurrentSessionChoice(t *testing.T) {
	h := newHarness(t)
	h.ctrl.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5", EffortLevels: []string{"low", "high"}},
	}
	h.ctrl.sessions = []controllerapi.SessionInfo{{
		ID: 11, ProjectID: chatProjectID, Model: "openai/gpt-5", ReasoningLevel: "high",
	}}
	openChat(t, h.dial(t))

	var res ModelsResult
	require.NoError(t, h.dial(t).Call(
		context.Background(), OpChatModels, SessionParams{SessionID: 11}, &res,
	))

	assert.Equal(t, h.ctrl.models, res.Models)
	assert.Equal(t, "openai/gpt-5", res.CurrentID)
	assert.Equal(t, "high", res.CurrentEffort)
}

func TestChatSetModel_UsesTheControllerSwitchPrimitive(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}
	client := h.dial(t)
	openChat(t, client)
	p := SetModelParams{SessionID: 11, Model: "openai/gpt-5", ReasoningLevel: "high"}

	require.NoError(t, client.Call(context.Background(), OpChatSetModel, p, nil))

	assert.Equal(t, []controllerapi.SessionSetModelData{{
		SessionID: 11, Model: "openai/gpt-5", ReasoningLevel: "high",
	}}, h.ctrl.setModel)
}

func TestChatOperationsRejectForeignSessionIDs(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}
	client := h.dial(t)
	openChat(t, client)

	tests := []struct {
		name   string
		op     string
		params any
	}{
		{name: "send", op: OpChatSend, params: SendParams{SessionID: 99, Text: "not yours"}},
		{name: "stop", op: OpChatStop, params: SessionParams{SessionID: 99}},
		{name: "models", op: OpChatModels, params: SessionParams{SessionID: 99}},
		{name: "set model", op: OpChatSetModel, params: SetModelParams{SessionID: 99, Model: "openai/gpt-5"}},
		{name: "cancel secret", op: OpChatSecretCancel, params: SecretCancelParams{SessionID: 99, RequestID: "req-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.Call(context.Background(), tt.op, tt.params, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not owned by the local chat")
		})
	}

	assert.Empty(t, h.ctrl.sent)
	assert.Empty(t, h.ctrl.setModel)
	assert.Empty(t, h.secrets.cancels())
}

// A conversation continues whatever state it ended in — a session that errored
// out is still the conversation, and restarting it would throw away the context
// the user is about to refer to.
func TestChatOpen_ResumesTheMostRecentSessionWhateverItsState(t *testing.T) {
	h := newHarness(t)
	killed := time.Now()

	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: 1, ProjectID: chatProjectID, UpdatedAt: time.Now().Add(-time.Hour), Status: "idle"},
		{ID: 2, ProjectID: chatProjectID, UpdatedAt: time.Now(), Status: "error"},
		{ID: 3, ProjectID: chatProjectID, UpdatedAt: time.Now().Add(time.Hour), KilledAt: &killed},
		{ID: 4, ProjectID: 99, UpdatedAt: time.Now().Add(2 * time.Hour)},
	}

	res := openChat(t, h.dial(t))
	assert.Equal(t, int64(2), res.SessionID, "newest live session in the chat project, error state and all")
}

func TestChatSend_UsesOneDurableMessagePath(t *testing.T) {
	tests := []struct {
		name    string
		running bool
	}{
		{name: "idle session"},
		{name: "running session", running: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.ctrl.sessions = []controllerapi.SessionInfo{
				{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now(), HasActiveLoop: tt.running},
			}

			c := h.dial(t)
			openChat(t, c)
			sendChat(t, c, SendParams{SessionID: 11, Text: "carry on"})

			require.Len(t, h.ctrl.sent, 1)
			assert.Equal(t, "carry on", h.ctrl.sent[0].Message)
		})
	}
}

func TestChatStop(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}

	c := h.dial(t)
	openChat(t, c)

	require.NoError(t, c.Call(context.Background(), OpChatStop, SessionParams{SessionID: 11}, nil))
	require.Len(t, h.ctrl.sent, 1)
	assert.Equal(t, "/stop", h.ctrl.sent[0].Message)
	assert.Equal(t, int64(11), h.ctrl.sent[0].SessionID)
}

// Concurrent terminals are allowed and both see the ephemeral activity stream
// of their own session only.
func TestChatEvents_ReachEveryAttachedTerminalAndNobodyElsesSession(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}

	first, second := h.dial(t), h.dial(t)
	openChat(t, first)
	openChat(t, second)

	// A different session's event is not this manager's business.
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    99,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyHeartbeat},
	}
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    11,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyHeartbeat},
	}

	for _, c := range []*ctl.Client{first, second} {
		event := waitForEvent(t, c)
		assert.Equal(t, int64(11), event.SessionID)
		assert.Equal(t, "heartbeat", event.Type, "the other session's event was filtered out")
	}
}

// A terminal that reconnects re-attaches to the same conversation — which is
// exactly what happens after the agent applies a config change.
func TestChatOpen_ReAttachesAfterAReconnect(t *testing.T) {
	h := newHarness(t)

	first := h.dial(t)
	openChat(t, first)

	created := sendChat(t, first, SendParams{Text: "start"})
	require.NoError(t, first.Close())

	reopened := openChat(t, h.dial(t))
	assert.Equal(t, created.SessionID, reopened.SessionID)
}

func TestChatSend_RefusesAnEmptyMessage(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t)

	openChat(t, c)

	err := c.Call(context.Background(), OpChatSend, SendParams{Text: "   "}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to send")
}

func waitForEvent(t *testing.T, c *ctl.Client) Event {
	t.Helper()

	select {
	case n := <-c.Notifications():
		require.Equal(t, EventMethod, n.Method)

		var e Event
		require.NoError(t, json.Unmarshal(n.Params, &e))

		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no chat event arrived")
	}

	return Event{}
}

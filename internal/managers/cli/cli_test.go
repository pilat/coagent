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
	assert.Equal(t, "/projects/coagent", res.WorkDir)
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
	assert.Equal(t, "/projects/coagent", h.ctrl.created[0].WorkDir)
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
	assert.Equal(t, []int64{11}, h.ctrl.stopped)
}

// Concurrent terminals are allowed and both see the whole conversation.
func TestChatEvents_ReachEveryAttachedTerminalAndNobodyElsesSession(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}

	first, second := h.dial(t), h.dial(t)
	openChat(t, first)
	openChat(t, second)

	// A different session's event is not this manager's business.
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    99,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "not yours"},
	}
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    11,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hello there"},
	}

	for _, c := range []*ctl.Client{first, second} {
		event := waitForEvent(t, c)
		assert.Equal(t, int64(11), event.SessionID)
		assert.Equal(t, "hello there", event.Message, "the other session's event was filtered out")
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

package cli

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

// fakeSecrets is the daemon's ledger of open masked prompts: a request stays
// until somebody answers or declines it, and only the first of those wins.
type fakeSecrets struct {
	mu        sync.Mutex
	open      map[int64][]sessionevent.Notification
	cancelled []string
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{open: make(map[int64][]sessionevent.Notification)}
}

func (f *fakeSecrets) ask(sessionID int64, n sessionevent.Notification) {
	f.mu.Lock()
	defer f.mu.Unlock()

	n.Type = sessionevent.NotifySecretRequest
	f.open[sessionID] = append(f.open[sessionID], n)
}

func (f *fakeSecrets) PendingSecretRequests(sessionID int64) []sessionevent.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]sessionevent.Notification(nil), f.open[sessionID]...)
}

func (f *fakeSecrets) CancelSecretRequest(_ context.Context, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for sessionID, open := range f.open {
		for i, n := range open {
			if n.RequestID != requestID {
				continue
			}

			f.open[sessionID] = append(open[:i], open[i+1:]...)
			f.cancelled = append(f.cancelled, requestID)

			return nil
		}
	}

	return assert.AnError
}

func (f *fakeSecrets) cancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.cancelled...)
}

// A masked prompt is pushed once, so a terminal attaching later must be asked
// again — otherwise its messages queue behind a call nothing on screen explains.
func TestScenario_ReattachedTerminalIsAskedForTheOutstandingSecret(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}
	//nolint:gosec // a variable name, never a value
	h.secrets.ask(11, sessionevent.Notification{
		RequestID:  "req-1",
		SecretName: "MANAGER_TG_BOT_TOKEN",
		Message:    "the bot token from BotFather",
	})

	first := h.dial(t)
	require.Equal(t, int64(11), openChat(t, first).SessionID)

	asked := waitForSecretRequest(t, first)
	assert.Equal(t, "req-1", asked.RequestID)
	require.NoError(t, first.Close())

	// The terminal that was asked walked away; a fresh one must see the prompt.
	second := h.dial(t)
	openChat(t, second)

	replayed := waitForSecretRequest(t, second)
	assert.Equal(t, "req-1", replayed.RequestID)
	assert.Equal(t, "MANAGER_TG_BOT_TOKEN", replayed.Name)
	assert.Equal(t, "the bot token from BotFather", replayed.Purpose)
	assert.Equal(t, int64(11), replayed.SessionID)

	require.NoError(t, second.Call(
		context.Background(), OpChatSecretCancel, SecretCancelParams{SessionID: 11, RequestID: "req-1"}, nil,
	))
	assert.Equal(t, []string{"req-1"}, h.secrets.cancels())

	// Two terminals may both hold the prompt: the second answer is refused.
	err := second.Call(
		context.Background(), OpChatSecretCancel, SecretCancelParams{SessionID: 11, RequestID: "req-1"}, nil,
	)
	require.Error(t, err)

	// A declined prompt is not replayed: the next terminal sees the conversation
	// and nothing else, which is what "before this event" pins down.
	third := h.dial(t)
	openChat(t, third)

	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    11,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "carry on"},
	}

	assert.Equal(t, "carry on", waitForEvent(t, third).Message)
}

// Two terminals can hold the same prompt open — the push went to everyone and the
// replay re-opens it. The one that does not win has to be told, or it sits at a
// masked prompt swallowing whatever is typed next.
func TestScenario_ResolvedSecretDismissesTheOtherTerminalsPrompt(t *testing.T) {
	h := newHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{{ID: 11, ProjectID: chatProjectID, UpdatedAt: time.Now()}}
	//nolint:gosec // a variable name, never a value
	h.secrets.ask(11, sessionevent.Notification{
		RequestID:  "req-1",
		SecretName: "MANAGER_TG_BOT_TOKEN",
		Message:    "the bot token from BotFather",
	})

	loser := h.dial(t)
	require.Equal(t, int64(11), openChat(t, loser).SessionID)
	require.Equal(t, "req-1", waitForSecretRequest(t, loser).RequestID)

	winner := h.dial(t)
	openChat(t, winner)
	require.Equal(t, "req-1", waitForSecretRequest(t, winner).RequestID)

	require.NoError(t, winner.Call(
		context.Background(), OpChatSecretCancel, SecretCancelParams{SessionID: 11, RequestID: "req-1"}, nil,
	))

	// The daemon publishes the dismissal once; the manager owes it to every
	// attached terminal, ordered behind the prompt it closes.
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID: 11,
		//nolint:gosec // a variable name, never a value
		Notification: sessionevent.Notification{
			Type:       sessionevent.NotifySecretResolved,
			RequestID:  "req-1",
			SecretName: "MANAGER_TG_BOT_TOKEN",
		},
	}
	h.ctrl.events <- controllerapi.SessionNotification{
		SessionID:    11,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "carry on"},
	}

	dismissed := waitForSecretResolved(t, loser)
	assert.Equal(t, "req-1", dismissed.RequestID)
	assert.Equal(t, "MANAGER_TG_BOT_TOKEN", dismissed.Name)
	assert.Equal(t, int64(11), dismissed.SessionID)

	// The next push is the ordinary message: the dismissal arrived exactly once.
	assert.Equal(t, "carry on", waitForEvent(t, loser).Message)
	assert.Equal(t, "req-1", waitForSecretResolved(t, winner).RequestID, "the winner is told too, and ignores it")
}

func waitForSecretResolved(t *testing.T, c *ctl.Client) SecretResolved {
	t.Helper()

	select {
	case n := <-c.Notifications():
		require.Equal(t, SecretResolvedMethod, n.Method)

		var res SecretResolved
		require.NoError(t, json.Unmarshal(n.Params, &res))

		return res
	case <-time.After(3 * time.Second):
		t.Fatal("no dismissal reached the terminal")
	}

	return SecretResolved{}
}

func waitForSecretRequest(t *testing.T, c *ctl.Client) SecretRequest {
	t.Helper()

	select {
	case n := <-c.Notifications():
		require.Equal(t, SecretRequestMethod, n.Method)

		var req SecretRequest
		require.NoError(t, json.Unmarshal(n.Params, &req))

		return req
	case <-time.After(3 * time.Second):
		t.Fatal("no masked prompt reached the terminal")
	}

	return SecretRequest{}
}

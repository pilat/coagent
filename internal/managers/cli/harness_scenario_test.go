package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

const (
	chatSessionID  int64 = 11
	otherSessionID int64 = 99
)

// scriptedController is the fake controller with a hook inside CreateSession:
// the daemon publishes for a session it has just started *before* it hands the
// id back, and that ordering is what these scenarios reproduce.
type scriptedController struct {
	*fakeController

	onCreate func(id int64)
}

func (s *scriptedController) CreateSession(ctx context.Context, d controllerapi.SessionCreateData) (int64, error) {
	id, err := s.fakeController.CreateSession(ctx, d)
	if err != nil || s.onCreate == nil {
		return id, err
	}

	s.onCreate(id)

	return id, nil
}

// chatHarness wires the real control server, the real chat manager and real
// dialled clients over one temp unix socket. Only the controller is scripted.
type chatHarness struct {
	ctrl   *scriptedController
	mgr    *Manager
	socket string
}

func newChatHarness(t *testing.T) *chatHarness {
	t.Helper()

	socket := scenarioSocket(t)

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{})
	require.NoError(t, err)

	t.Cleanup(func() { _ = srv.Close() })

	ctrl := &scriptedController{fakeController: newFakeController()}
	// Unbuffered: a publish returns only once the forwarder has taken the event,
	// which is what makes the ordering assertions deterministic.
	ctrl.events = make(chan controllerapi.SessionNotification)

	mgr := New(ctrl, srv, "claude-sonnet-5", newFakeSecrets())
	require.NoError(t, mgr.Start(context.Background()))

	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	go func() { _ = srv.Serve(context.Background()) }()

	return &chatHarness{ctrl: ctrl, mgr: mgr, socket: socket}
}

// scenarioSocket keeps the unix path under the ~100-byte sun_path limit, which a
// plain t.TempDir() join does not guarantee on a deep TMPDIR.
func scenarioSocket(t *testing.T) string {
	t.Helper()

	if p := filepath.Join(t.TempDir(), "d.sock"); len(p) <= 100 {
		return p
	}

	short, err := os.MkdirTemp("/tmp", "cliscn")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	return filepath.Join(short, "d.sock")
}

func (h *chatHarness) dial(t *testing.T) *ctl.Client {
	t.Helper()

	c, err := ctl.Dial(context.Background(), h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func (h *chatHarness) publish(t *testing.T, sn controllerapi.SessionNotification) {
	t.Helper()

	select {
	case h.ctrl.events <- sn:
	case <-time.After(3 * time.Second):
		t.Fatal("the manager stopped consuming session events")
	}
}

// settle returns once the forwarder has finished handling everything published
// before it: the forwarder is sequential, so taking the next event proves the
// previous one was fully published.
func (h *chatHarness) settle(t *testing.T) {
	t.Helper()

	h.publish(t, chatEvent(otherSessionID, "barrier"))
}

func (h *chatHarness) waitForAttached(t *testing.T, want int) {
	t.Helper()

	require.Eventually(t, func() bool {
		h.mgr.mu.Lock()
		defer h.mgr.mu.Unlock()

		return len(h.mgr.clients) == want
	}, 3*time.Second, 10*time.Millisecond, "attached terminal count")
}

func (h *chatHarness) sentMessages() []controllerapi.SessionMessageData {
	h.ctrl.mu.Lock()
	defer h.ctrl.mu.Unlock()

	return append([]controllerapi.SessionMessageData(nil), h.ctrl.sent...)
}

func TestHarnessScenario_CLIRendersLifecycleGatedBudgetReadiness(t *testing.T) {
	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}
	client := h.dial(t)
	opened := openChat(t, client)
	require.Equal(t, chatSessionID, opened.SessionID)
	h.waitForAttached(t, 1)

	h.publish(t, controllerapi.SessionNotification{
		SessionID: opened.SessionID,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "budget reached",
		},
	})
	h.settle(t)

	event := waitForEvent(t, client)
	assert.Equal(t, Event{SessionID: opened.SessionID, Type: "state_changed", Status: "idle"}, event)
}

// chatEvent rides the ephemeral heartbeat channel: these tests exercise the
// terminal socket mechanics, not durable output, which has its own scenarios.
func chatEvent(sessionID int64, message string) controllerapi.SessionNotification {
	return controllerapi.SessionNotification{
		SessionID:    sessionID,
		Notification: sessionevent.Notification{Type: sessionevent.NotifyHeartbeat, Message: message},
	}
}

func requireNoEvent(t *testing.T, c *ctl.Client) {
	t.Helper()

	select {
	case n, ok := <-c.Notifications():
		require.True(t, ok, "connection closed")
		t.Fatalf("unexpected push %s: %s", n.Method, n.Params)
	case <-time.After(150 * time.Millisecond):
	}
}

// A terminal sees the chat session's events, in order, and nothing else — a
// dialled client that never opened the chat is not a terminal.
func TestHarnessScenario_ChatEventsReachAttachedTerminalsOnlyInPublishedOrder(t *testing.T) {
	const lines = 5

	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	first, second, bystander := h.dial(t), h.dial(t), h.dial(t)
	openChat(t, first)
	openChat(t, second)

	h.publish(t, chatEvent(otherSessionID, "not yours"))

	for i := range lines {
		h.publish(t, chatEvent(chatSessionID, fmt.Sprintf("line %d", i)))
	}

	h.settle(t)

	for _, c := range []*ctl.Client{first, second} {
		for i := range lines {
			event := waitForEvent(t, c)
			assert.Equal(t, chatSessionID, event.SessionID)
			assert.Equal(t, fmt.Sprintf("line %d", i), event.Message)
		}
	}

	requireNoEvent(t, bystander)
}

// Responses and pushes share one connection. Concurrent calls must each get
// their own answer back while the event stream keeps flowing.
func TestHarnessScenario_ConcurrentSendsAndPushesNeverCrossWire(t *testing.T) {
	const (
		senders = 24
		lines   = 20
	)

	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	c := h.dial(t)
	openChat(t, c)

	var (
		wg      sync.WaitGroup
		results = make([]SendResult, senders)
		errs    = make([]error, senders)
	)

	for i := range senders {
		wg.Go(func() {
			sessionID := chatSessionID
			if i%2 == 1 {
				// Distinct rejected ids make a crossed error response visible while
				// preserving the manager ownership boundary.
				sessionID = int64(1000 + i)
			}
			params := SendParams{SessionID: sessionID, Text: fmt.Sprintf("msg %d", i)}
			errs[i] = c.Call(context.Background(), OpChatSend, params, &results[i])
		})
	}

	for i := range lines {
		h.publish(t, chatEvent(chatSessionID, fmt.Sprintf("line %d", i)))
	}

	wg.Wait()

	for i := range senders {
		if i%2 == 1 {
			require.Error(t, errs[i])
			assert.Contains(t, errs[i].Error(), fmt.Sprintf("session %d", 1000+i))
			continue
		}

		require.NoError(t, errs[i])
		assert.Equal(t, chatSessionID, results[i].SessionID)
	}

	sentTexts := map[string]bool{}
	for _, sent := range h.sentMessages() {
		assert.Equal(t, chatSessionID, sent.SessionID)
		sentTexts[sent.Message] = true
	}

	require.Len(t, sentTexts, senders/2, "every owned message reached the controller exactly once")

	for i := range senders {
		assert.Equal(t, i%2 == 0, sentTexts[fmt.Sprintf("msg %d", i)])
	}

	for i := range lines {
		assert.Equal(t, fmt.Sprintf("line %d", i), waitForEvent(t, c).Message, "no event lost or reordered")
	}
}

// One terminal dropping mid-stream is routine — the agent restarts the daemon
// under them. The survivors keep their stream and a reconnect re-attaches.
func TestHarnessScenario_ADroppedTerminalLeavesTheOthersServed(t *testing.T) {
	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	first, second := h.dial(t), h.dial(t)
	openChat(t, first)
	openChat(t, second)
	h.waitForAttached(t, 2)

	h.publish(t, chatEvent(chatSessionID, "before"))
	assert.Equal(t, "before", waitForEvent(t, first).Message)
	assert.Equal(t, "before", waitForEvent(t, second).Message)

	require.NoError(t, first.Close())
	h.waitForAttached(t, 1)

	h.publish(t, chatEvent(chatSessionID, "after"))
	assert.Equal(t, "after", waitForEvent(t, second).Message)

	third := h.dial(t)
	require.Equal(t, chatSessionID, openChat(t, third).SessionID)
	h.waitForAttached(t, 2)

	h.publish(t, chatEvent(chatSessionID, "rejoined"))
	assert.Equal(t, "rejoined", waitForEvent(t, second).Message)
	assert.Equal(t, "rejoined", waitForEvent(t, third).Message)
}

// The wire is newline-delimited, so a payload larger than either side's read
// buffer — and one carrying newlines of its own — has to survive intact.
func TestHarnessScenario_LargeChatFramesSurviveTheNewlineCodec(t *testing.T) {
	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	c := h.dial(t)
	openChat(t, c)

	text := strings.Repeat("край\nof the\tworld \"quoted\" {json}\r\n", 2000)

	sendChat(t, c, SendParams{SessionID: chatSessionID, Text: text})

	sent := h.sentMessages()
	require.Len(t, sent, 1)
	require.Len(t, sent[0].Message, len(text))
	require.Equal(t, text, sent[0].Message, "the request survived the codec")

	h.publish(t, chatEvent(chatSessionID, text))

	got := waitForEvent(t, c).Message
	require.Len(t, got, len(text))
	require.Equal(t, text, got, "the push survived the codec")
}

// The daemon publishes for a session it has just started before CreateSession
// hands the id back. That event belongs to the terminal that asked for it.
func TestHarnessScenario_EventsPublishedBeforeTheSessionIDIsKnownStillReachTheTerminal(t *testing.T) {
	h := newChatHarness(t)

	c := h.dial(t)
	openChat(t, c)

	h.ctrl.onCreate = func(id int64) {
		// The barrier returns only once the forwarder has finished publishing the
		// first event, so the race is decided before CreateSession returns.
		h.ctrl.events <- chatEvent(id, "first line")
		h.ctrl.events <- chatEvent(otherSessionID, "barrier")
	}

	created := sendChat(t, c, SendParams{Text: "hello"})
	require.NotZero(t, created.SessionID)

	h.publish(t, chatEvent(created.SessionID, "second line"))

	for _, want := range []string{"first line", "second line"} {
		event := waitForEvent(t, c)
		assert.Equal(t, created.SessionID, event.SessionID)
		assert.Equal(t, want, event.Message, "a released event still precedes the live one")
	}
}

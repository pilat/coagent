package cli

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

// wedgeTerminal attaches a raw connection that never reads its socket again —
// what a suspended or hung terminal looks like from the daemon's side — and
// returns the terminal the manager made for it.
func (h *chatHarness) wedgeTerminal(t *testing.T) *terminal {
	t.Helper()

	before := h.attached()

	conn, err := net.Dial("unix", h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":%q}\n", OpChatOpen)
	require.NoError(t, err)

	var wedged *terminal

	require.Eventually(t, func() bool {
		for _, candidate := range h.attached() {
			if !slices.Contains(before, candidate) {
				wedged = candidate

				return true
			}
		}

		return false
	}, 3*time.Second, 10*time.Millisecond, "the wedged terminal attached")

	return wedged
}

func (h *chatHarness) attached() []*terminal {
	h.mgr.mu.Lock()
	defer h.mgr.mu.Unlock()

	return slices.Clone(h.mgr.clients)
}

// requireWriterExited proves the dropped terminal's writer goroutine is gone,
// not merely unsubscribed — the whole point of dropping it.
func requireWriterExited(t *testing.T, term *terminal) {
	t.Helper()

	select {
	case <-term.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the dropped terminal's writer is still parked in a socket write")
	}
}

// A terminal that stops draining its socket must cost only itself; before the
// per-connection queue it stalled every other terminal inside one socket write.
func TestHarnessScenario_AWedgedTerminalNeverStallsTheHealthyOnes(t *testing.T) {
	// Enough to outlast the wedged socket's kernel buffer plus a full queue, with
	// payloads large enough that the buffer is measured in tens of events.
	const (
		lines  = terminalQueueCap + 96
		filler = 16 << 10
	)

	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	healthy := h.dial(t)
	openChat(t, healthy)
	h.waitForAttached(t, 1)

	wedged := h.wedgeTerminal(t)
	h.waitForAttached(t, 2)

	// Each line is published only once the previous one has landed, so the healthy
	// terminal is never itself behind — only the wedged one accumulates.
	padding := strings.Repeat("x", filler)

	for i := range lines {
		want := fmt.Sprintf("line %d %s", i, padding)
		h.publish(t, chatEvent(chatSessionID, want))

		got := waitForEvent(t, healthy)
		require.Equal(t, chatSessionID, got.SessionID)
		require.Equal(t, want, got.Message, "the healthy terminal keeps the complete ordered trace")
	}

	h.waitForAttached(t, 1)
	requireWriterExited(t, wedged)

	// The forwarder is still live, and the terminal it dropped stays dropped.
	h.publish(t, chatEvent(chatSessionID, "after"))
	assert.Equal(t, "after", waitForEvent(t, healthy).Message)
}

// With every terminal wedged nobody paces the forwarder, so the publishes are the
// assertion: the manager keeps draining instead of parking in a socket write.
func TestHarnessScenario_EveryTerminalWedgedStillDrainsTheSubscription(t *testing.T) {
	const (
		lines  = terminalQueueCap + 96
		filler = 16 << 10
	)

	h := newChatHarness(t)
	h.ctrl.sessions = []controllerapi.SessionInfo{
		{ID: chatSessionID, ProjectID: chatProjectID, UpdatedAt: time.Now()},
	}

	first, second := h.wedgeTerminal(t), h.wedgeTerminal(t)
	h.waitForAttached(t, 2)

	padding := strings.Repeat("x", filler)
	for i := range lines {
		h.publish(t, chatEvent(chatSessionID, fmt.Sprintf("line %d %s", i, padding)))
	}

	h.waitForAttached(t, 0)
	requireWriterExited(t, first)
	requireWriterExited(t, second)
}

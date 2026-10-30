package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantCommandsWithoutHumanAuthority(t *testing.T) {
	for _, tc := range []struct{ name, command, state string }{
		{"config", "/config show me", "idle"},
		{"budget", "/budget 5$", "idle"},
		{"parked budget", "/budget 5$", "parked"},
		{"stop running", "/stop", "running"},
		{"kill stopped", "/kill", "stopped"},
		{"status idle", "/status", "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var workID int64
			started, release := make(chan struct{}), make(chan struct{})
			h := newHarness(
				t,
				harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
					if strings.Contains(system, "You are Coagent, the personal assistant") {
						if hasToolResultFor(messages, "send") || hasUserContaining(messages, "←") {
							return textReply("command delivered")
						}
						return callReply(
							"send-command",
							"send",
							fmt.Sprintf(`{"session":%d,"text":%q}`, workID, tc.command),
						)
					}
					if hasUserContaining(messages, "later input") {
						return textReply("later input handled")
					}
					if hasUserContaining(messages, "hold work") {
						closeOnce(started)
						<-release
					}
					return textReply("work accepted command")
				}},
			)
			defer func() {
				closeOnce(release)
				h.shutdown()
			}()
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			workID = h.createRoot(managerAttrs(scenarioManagerID))
			prepareAssistantCommandTarget(t, h, root.ID, workID, tc.state, started)
			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "relay the command"))
			h.waitUntil("assistant command finishes without a tree-lock deadlock", func() bool {
				var count int
				require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM messages
					WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'send-command'`, root.ID).Scan(&count))
				return count == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			var result string
			require.NoError(
				t,
				h.db.QueryRowContext(h.ctx, `SELECT content FROM messages
					WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'send-command'`, root.ID).Scan(&result),
			)
			if tc.state == "parked" {
				assert.Contains(
					t,
					result,
					fmt.Sprintf(
						"session #%d is parked at its budget checkpoint; raise or clear its budget with set_budget first",
						workID,
					),
				)
				record, loadErr := h.store.Get(h.ctx, workID)
				require.NoError(t, loadErr)
				assert.Equal(t, budget.Fired, record.State)
				pending, loadErr := h.store.ListPending(h.ctx, workID)
				require.NoError(t, loadErr)
				assert.Empty(t, pending)
				return
			}
			assert.Contains(t, result, fmt.Sprintf("sent to session %d", workID))
			assertAssistantCommandOutcome(t, h, workID, tc.command)
			var grants int
			require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_tool_activations
				WHERE session_id = ?`, workID).Scan(&grants))
			assert.Zero(t, grants)
		})
	}
}

func prepareAssistantCommandTarget(
	t *testing.T,
	h *harness,
	assistant, work int64,
	state string,
	started <-chan struct{},
) {
	t.Helper()
	if state == "running" {
		require.NoError(t, h.mgr.sendToSession(h.ctx, work, "hold work"))
		requireSignal(t, started)
		return
	}
	if state == "parked" {
		armed, err := h.store.Arm(h.ctx, foreignBudgetMutation(assistant, work, "arm-command-test"))
		require.NoError(t, err)
		fired, _, err := h.store.FireBudget(h.ctx, work, armed.Generation, "duration", 0, "checkpoint")
		require.NoError(t, err)
		_, err = h.store.BeginBudgetDrain(h.ctx, work, fired.Generation, fired.ParkOwner)
		require.NoError(t, err)
		_, err = h.store.MarkBudgetParked(h.ctx, work, fired.Generation, fired.ParkOwner)
		require.NoError(t, err)
	}
	if state == "stopped" || state == "parked" {
		require.NoError(t, h.store.UpdateSessionStatus(h.ctx, work, sessionstore.SessionStatusStopped))
	}
}

func assertAssistantCommandOutcome(t *testing.T, h *harness, work int64, command string) {
	t.Helper()
	if command == "/config show me" || command == "/budget 5$" {
		h.waitUntil("work processes command as text", func() bool {
			return assistantSayCount(t, h, work, "work accepted command") == 1 && !h.mgr.HasActiveLoop(work)
		})
		found := false
		for _, message := range h.messages(work) {
			assert.NotContains(t, message.Content, "Host activation")
			found = found || (message.Role == llmwire.RoleUser && strings.Contains(message.Content, command))
		}
		assert.True(t, found)
		return
	}
	var inputState string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state FROM session_inbox
		WHERE session_id = ? AND raw_content = ?`, work, command).Scan(&inputState))
	assert.Equal(t, "handled", inputState)
	var outputs []string
	for _, row := range h.outbox(work) {
		outputs = append(outputs, row.Content)
	}
	switch command {
	case "/status":
		assert.Contains(t, strings.Join(outputs, "\n"), "## Session progress")
		assert.False(t, h.mgr.HasActiveLoop(work))
	case "/kill":
		assert.NotNil(t, h.session(work).KilledAt)
		assert.False(t, h.mgr.HasActiveLoop(work))
		assert.Empty(t, h.messages(work), "a stopped target must not restart on /kill")
	case "/stop":
		assert.Contains(t, outputs, "⏳ Stopping…")
		assert.Equal(t, sessionstore.SessionStatusStopped, h.session(work).Status)
		require.NoError(t, h.mgr.sendToSession(h.ctx, work, "later input"))
		h.waitUntil("stopped root accepts later work", func() bool {
			return assistantSayCount(t, h, work, "later input handled") == 1 && !h.mgr.HasActiveLoop(work)
		})
	}
}

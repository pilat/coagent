package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_AssistantSchedulesUsesCurrentSnapshot(t *testing.T) {
	var workID int64
	created, release, continued, finish := make(
		chan struct{},
	), make(
		chan struct{},
	), make(
		chan struct{},
	), make(
		chan struct{},
	)
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if strings.Contains(system, "You are Coagent, the personal assistant") {
			if hasToolResultFor(messages, "send") || hasUserContaining(messages, "←") {
				return textReply("schedule query sent")
			}

			return callReply("query-schedules", "send", fmt.Sprintf(`{"session":%d,"text":"/schedules"}`, workID))
		}
		if !hasToolResultFor(messages, tool.IDSchedule) {
			return callReply("create-schedule", tool.IDSchedule,
				`{"action":"create","cron":"0 9 * * *","prompt":"freshly created reminder"}`)
		}
		if !hasToolResultFor(messages, "ls") {
			closeOnce(created)
			<-release

			return callReply("keep-working", "ls", `{"path":"."}`)
		}

		closeOnce(continued)
		<-finish

		return textReply("work finished")
	}})
	defer func() {
		closeOnce(release)
		closeOnce(finish)
		h.shutdown()
	}()
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	workID = h.createRoot(managerAttrs(scenarioManagerID))
	require.NoError(t, h.mgr.sendToSession(h.ctx, workID, "create a reminder and continue working"))
	requireSignal(t, created)
	original, running := h.mgr.runners.load(workID)
	require.True(t, running)
	want, err := h.mgr.schedules.Render(h.ctx, workID)
	require.NoError(t, err)
	require.Contains(t, want, "freshly created reminder")

	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "show the work session's schedules"))
	h.waitUntil("assistant enqueues schedules on the running work session", func() bool {
		var count int
		require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM messages
			WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'query-schedules'`, root.ID).Scan(&count))

		return count == 1
	})
	var inputID int64
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT id FROM session_inbox
		WHERE session_id = ? AND raw_content = '/schedules'`, workID).Scan(&inputID))
	closeOnce(release)
	requireSignal(t, continued)
	current, running := h.mgr.runners.load(workID)
	require.True(t, running)
	require.Same(t, original, current)

	var got string
	h.waitUntil("schedules command produces durable output", func() bool {
		for _, row := range h.outbox(workID) {
			if row.SourceKey == fmt.Sprintf("input:%d:schedules:result", inputID) {
				got = row.Content

				return true
			}
		}

		return false
	})
	assert.Equal(t, want, got)
	collector.waitMessage(workID, got)
	closeOnce(finish)
	collector.waitMessage(workID, "work finished")
	assert.Equal(t, 1, countPublishedMessage(collector.snapshot(), workID, want))
}

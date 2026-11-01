package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/schedule"
)

func TestHarnessScenario_AssistantWorkScheduleReport(t *testing.T) {
	for _, report := range []bool{false, true} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("report=%t/fresh=%t", report, fresh), func(t *testing.T) {
				h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
					if strings.HasPrefix(system, registry.AssistantPrompt) {
						return textReply("")
					}
					return textReply("work schedule answer")
				}})
				root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
				require.NoError(t, err)
				work := h.createRoot(managerAttrs(scenarioManagerID))
				raw, err := json.Marshal(map[string]any{
					"action":  "create",
					"session": work,
					"at":      time.Now().Add(time.Hour).In(time.Local).Format("2006-01-02 15:04"),
					"prompt":  "scheduled inspection",
					"report":  report,
					"fresh":   fresh,
				})
				require.NoError(t, err)
				_, err = h.mgr.assistantScheduleTool(assistantFor(h, root.ID).scope).Execute(h.ctx, raw)
				require.NoError(t, err)
				_, err = h.db.ExecContext(
					h.ctx,
					`UPDATE schedules SET one_shot_at = ? WHERE session_id = ?`,
					time.Now().Add(-time.Minute),
					work,
				)
				require.NoError(t, err)
				h.startInboxWake()
				executor := schedule.NewExecutor(h.schedules, h.mgr)
				executor.Start(h.ctx)
				t.Cleanup(executor.Stop)
				h.waitUntil("scheduled work complete", func() bool {
					return assistantSayCount(t, h, work, "work schedule answer") == 1 && !h.mgr.HasActiveLoop(work)
				})
				if report {
					waitAssistantObservation(t, h, root.ID, 1)
					rows := assistantChatMessages(t, h, root.ID)
					require.Len(t, rows, 1)
					assert.Equal(t, work, rows[0].Origin)
					assert.Equal(t, "work schedule answer", rows[0].Content)
					assertAssistantOwnOutputCount(t, h, root.ID, 0)
					assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
					var event string
					require.NoError(
						t,
						h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox WHERE session_id = ? AND source = 'agent'`, root.ID).
							Scan(&event),
					)
					assert.Contains(t, event, fmt.Sprintf("⏰ %s (%d) schedule #", filepathBaseWorkDir(t, h), work))
					assert.Contains(t, event, `answered: "work schedule answer"`)
					assert.NotContains(t, event, "user wrote:")
				} else {
					assert.Zero(t, assistantEventCount(t, h, root.ID))
				}
			})
		}
	}
}

func filepathBaseWorkDir(t *testing.T, h *harness) string {
	t.Helper()
	name, err := h.store.GetProjectName(h.ctx, h.projectID)
	require.NoError(t, err)
	return name
}

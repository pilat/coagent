package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_AssistantProjectsExactlyDeskAndOwnerTools(t *testing.T) {
	h := newRegistryPromptHarness(t, trivialRespond)
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "show the available work"))
	h.waitUntil("assistant tool projection", func() bool {
		return assistantSayCount(t, h.harness, root.ID, "done") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assert.ElementsMatch(t, []string{
		"search", "around", "read_session", "sessions", "projects", "focus_list",
		"open", "send", "stop", "remember", "rewrite_memory", "forget_memory",
		"focus", "update_focus", "done_focus",
		tool.IDSchedule, tool.IDMCPAdd, tool.IDMCPRemove, tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPList,
		tool.IDConfigRead, tool.IDConfigEdit, "set_budget",
	}, h.schemas.first(t, root.ID))
}

func TestHarnessScenario_AssistantConfigReadEditRestartsAndSettlesVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assistant-config.db")
	configDir := newApplyConfigDir(t)
	respond := assistantConfigReadEditResponder(t)
	first := newApplyDaemonWith(t, path, configDir, respond)
	root, err := first.mgr.EnsureAssistantRoot(first.ctx, scenarioManagerID)
	require.NoError(t, err)
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, root.ID, "/config reorder the configured models"))
	first.waitUntil("assistant requests configuration restart", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("assistant config call suspended", func() bool { return !first.mgr.HasActiveLoop(root.ID) })
	assert.Equal(t, 1, countAssistantToolCallsFor(first.messages(root.ID), tool.IDConfigRead))
	assert.Equal(t, 1, countToolResultsFor(first.messages(root.ID), tool.IDConfigRead))
	assert.Equal(t, 1, countAssistantToolCallsFor(first.messages(root.ID), tool.IDConfigEdit))
	assert.Zero(t, countToolResultsFor(first.messages(root.ID), tool.IDConfigEdit))
	assert.Equal(t, "claude-opus-5", defaultModelInFile(t, configDir))
	first.shutdown()

	second := newApplyDaemonWith(t, path, configDir, respond)
	require.NoError(t, second.mgr.Start(second.ctx))
	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	assert.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	second.waitUntil("assistant receives apply verdict", func() bool {
		return assistantSayCount(t, second.harness, root.ID, "assistant configuration replaced") == 1 &&
			!second.mgr.HasActiveLoop(root.ID)
	})
	messages := second.messages(root.ID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countAssistantToolCallsFor(messages, tool.IDConfigRead))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDConfigRead))
	assert.Equal(t, 1, countAssistantToolCallsFor(messages, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDConfigEdit))
	activation := currentActivationOf(t, second.harness, root.ID)
	require.NotNil(t, activation)
	assert.Equal(t, sessionstore.ActivationConsumed, activation.State)
	assert.Equal(t, "assistant-edit", activation.ToolCallID)
	_, err = second.store.PendingActivation(second.ctx, root.ID)
	require.ErrorIs(t, err, sessionstore.ErrActivationNotFound)
	again, err := second.mgr.EnsureAssistantRoot(second.ctx, scenarioManagerID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, again.ID)
}

func TestHarnessScenario_AssistantRefusesUngatedConfigReadAndProjectMCP(t *testing.T) {
	for _, test := range []struct{ name, toolID, params, reason string }{
		{name: "config without activation", toolID: tool.IDConfigRead, params: `{}`, reason: "/config"},
		{
			name: "project MCP", toolID: tool.IDMCPAdd,
			params: `{"name":"test-server","scope":"project","command":"unused"}`, reason: "cannot use project scope",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasToolResultFor(messages, test.toolID) {
					return textReply("request refused")
				}
				return callReply("refused-owner-call", test.toolID, test.params)
			}})
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "attempt the requested operation"))
			h.waitUntil("assistant refuses authority or scope", func() bool {
				return assistantSayCount(t, h, root.ID, "request refused") == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			messages := h.messages(root.ID)
			assert.Equal(t, 1, countAssistantToolCallsFor(messages, test.toolID))
			assert.Contains(t, configLastToolResultContent(messages, test.toolID), test.reason)
		})
	}
}

func TestHarnessScenario_AssistantSchedulePinsFreshAndNormalDeskHeads(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "fresh"}[fresh], func(t *testing.T) {
			requests := make(chan []llmwire.Message, 1)
			var captured sync.Once
			h := newHarness(
				t,
				harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
					require.True(t, strings.HasPrefix(system, registry.AssistantPrompt))
					if hasUserContaining(messages, "assistant scheduled tick") ||
						hasToolResultFor(messages, tool.IDSchedule) {
						captured.Do(func() { requests <- slices.Clone(messages) })
						return textReply("assistant scheduled reply")
					}
					return textReply("assistant initialized")
				}},
			)
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "initialize assistant"))
			h.waitUntil("assistant initial exchange", func() bool {
				return assistantSayCount(t, h, root.ID, "assistant initialized") == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			due := time.Now().Add(-time.Minute)
			_, err = h.schedules.AddSchedule(h.ctx, root.ID, "", &due, "assistant scheduled tick", fresh)
			require.NoError(t, err)
			executor := schedule.NewExecutor(h.schedules, h.mgr)
			executor.Start(h.ctx)
			t.Cleanup(executor.Stop)
			h.waitUntil("assistant scheduled reply", func() bool {
				entries, err := h.schedules.ListSchedules(h.ctx, root.ID)
				return err == nil && len(entries) == 0 &&
					assistantSayCount(t, h, root.ID, "assistant scheduled reply") == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			executor.Stop()
			var messages []llmwire.Message
			select {
			case messages = <-requests:
			case <-time.After(time.Second):
				t.Fatal("no scheduled model request recorded")
			}
			require.NotEmpty(t, messages)
			assert.Contains(t, messages[0].Content, "## memory")
			assert.Contains(t, messages[0].Content, "initialize assistant")
			assert.Contains(t, messages[0].Content, "assistant initialized")
			assert.Equal(t, fresh, hasUserContaining(messages, "assistant scheduled tick"))
			assert.Equal(t, !fresh, hasToolResultFor(messages, tool.IDSchedule))
			var accepted int
			require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
				WHERE session_id = ? AND source = 'schedule' AND state = 'accepted'
				AND raw_content = 'assistant scheduled tick'`, root.ID).Scan(&accepted))
			assert.Equal(t, 1, accepted)
		})
	}
}

func assistantConfigReadEditResponder(t *testing.T) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()
	return func(system string, messages []llmwire.Message) *llmwire.Response {
		require.True(t, strings.HasPrefix(system, registry.AssistantPrompt))
		if hasToolResultFor(messages, tool.IDConfigEdit) {
			assert.Contains(t, configLastToolResultContent(messages, tool.IDConfigEdit), "Config applied")
			return textReply("assistant configuration replaced")
		}

		if hasToolResultFor(messages, tool.IDConfigRead) {
			assert.Equal(t, toolConfig, configLastToolResultContent(messages, tool.IDConfigRead))
			return callReply("assistant-edit", tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidate)+`}`)
		}

		return callReply("assistant-read", tool.IDConfigRead, `{}`)
	}
}

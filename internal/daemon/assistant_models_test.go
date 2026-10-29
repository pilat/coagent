package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

func TestAssistantHostModels(t *testing.T) {
	t.Parallel()
	host := assistantHost{s: &svc{models: models{entries: []config.ModelEntry{
		{
			ID:            "one",
			Name:          "name",
			DisplayName:   "display",
			Pricing:       &config.ModelPricing{InputPrice: 2, OutputPrice: 8},
			Reasoning:     &config.ReasoningSpec{Supported: true},
			EffortLevels:  []string{"low"},
			DefaultEffort: "low",
		},
		{ID: "two", Name: "name fallback", Reasoning: &config.ReasoningSpec{Supported: false}},
		{ID: "three"},
	}}}}
	entries, err := host.Models(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "display", entries[0].DisplayName)
	assert.Equal(t, "name fallback", entries[1].DisplayName)
	assert.Equal(t, "three", entries[2].DisplayName)
	assert.True(t, entries[0].Reasoning)
	assert.True(t, entries[0].Priced)
	assert.InDelta(t, 2, entries[0].InputPrice, 1e-9)
	assert.InDelta(t, 8, entries[0].OutputPrice, 1e-9)
	assert.Equal(t, []string{"low"}, entries[0].EffortLevels)
	assert.Equal(t, "low", entries[0].DefaultEffort)
	assert.False(t, entries[1].Reasoning)
	assert.False(t, entries[1].Priced)
}

func TestHarnessScenario_AssistantSwitch(t *testing.T) {
	for _, state := range []string{"bound", "unbound", "in work"} {
		t.Run(state, func(t *testing.T) {
			var workID int64
			h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasToolResultFor(messages, "switch") {
					return textReply("switch finished")
				}
				return callReply("enter-work", "switch", fmt.Sprintf(`{"session":%d}`, workID))
			}})
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			work := workWithAnswer(t, h, root.ID, false, "work", "answer")
			workID = work
			if state != "unbound" {
				require.NoError(t, h.store.BindChat(h.ctx, scenarioManagerID, "chat"))
			}
			current := work
			if state == "in work" {
				current = workWithAnswer(t, h, root.ID, false, "other work", "other answer")
				require.NoError(t, h.store.SetChatFocus(h.ctx, scenarioManagerID, "chat", current))
			}
			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "connect me to the work session"))
			h.waitUntil("assistant switches or refuses", func() bool {
				return assistantSayCount(t, h, root.ID, "switch finished") == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			result := configLastToolResultContent(h.messages(root.ID), "switch")
			switch state {
			case "bound":
				assert.Equal(
					t,
					fmt.Sprintf("switched the user to session %d; they return with /assistant", work),
					result,
				)
			case "unbound":
				assert.Contains(t, result, "chat is not bound yet")
			case "in work":
				assert.Contains(t, result, fmt.Sprintf("the user is in session #%d; they switch themselves", current))
			}
			focus, err := h.store.ChatFocus(h.ctx, scenarioManagerID, "chat")
			require.NoError(t, err)
			if state == "unbound" {
				assert.Zero(t, focus)
			} else {
				assert.Equal(t, current, focus)
			}
		})
	}
}

func TestHarnessScenario_AssistantSetsWorkModel(t *testing.T) {
	var workID int64
	h := newHarness(t, harnessOptions{
		configure: func(cfg *config.Config) {
			withKnownModels([]string{"fake-model", "second"})(cfg)
			cfg.UnifiedConfig.Models[1] = reasoningModelEntry("second", []string{"low", "medium"}, "medium")
		},
		respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, "set_model") {
				return textReply("model selected")
			}
			return callReply("model-work", "set_model", fmt.Sprintf(`{"session":%d,"model":"second"}`, workID))
		},
	})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	workID = workWithAnswer(t, h, root.ID, false, "work", "answer")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "use the second model for the work"))
	h.waitUntil("assistant selects work model", func() bool {
		return assistantSayCount(t, h, root.ID, "model selected") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assert.Contains(t, configLastToolResultContent(h.messages(root.ID), "set_model"), "now runs second · medium")
	record, err := h.store.GetSession(h.ctx, workID)
	require.NoError(t, err)
	assert.Equal(t, "second", record.Model)
	assert.Equal(t, "medium", record.ReasoningLevel)
}

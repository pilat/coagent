package telegram

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

var pickerModels = []controllerapi.ConfigModelInfo{
	{
		ID: "claude-opus-5", Name: "Claude Opus 5", DisplayName: "anthropic/Claude Opus 5",
		InputPrice: 5, OutputPrice: 25,
		EffortLevels: []string{"low", "medium", "high", "max"}, DefaultEffort: "medium",
	},
	{
		ID: "local/plain", Name: "Plain", DisplayName: "local/Plain",
		InputPrice: 0, OutputPrice: 0,
	},
}

func TestHandleModelRendersDisplayNamePriceAndEffort(t *testing.T) {
	var messages []string
	ctrl := &fakeController{
		focus: 42, listModels: pickerModels,
		listSessions: []controllerapi.SessionInfo{{
			ID: 42, Model: pickerModels[0].ID, ReasoningLevel: "high",
			Attributes: map[string]any{"manager_id": "telegram-main"},
		}},
	}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/model")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "<b>Model:</b> "+pickerModels[0].DisplayName+" · effort: high")
	assert.Contains(t, messages[0], "1. "+pickerModels[0].DisplayName+" · $5/$25 · default effort: medium ◀")
	assert.Contains(t, messages[0], "2. local/Plain · free")

	ctrl.listSessions[0].ReasoningLevel = ""
	m.handleChatText(t.Context(), "/model")
	assert.NotContains(t, messages[1], pickerModels[0].DisplayName+" · effort:")
}

func TestHandleModelMarksStaleCatalog(t *testing.T) {
	var messages []string
	models := append([]controllerapi.ConfigModelInfo(nil), pickerModels...)
	models[0].CatalogStale = true
	models[0].DefaultEffort = "high"
	ctrl := &fakeController{
		focus: 42, listModels: models,
		listSessions: []controllerapi.SessionInfo{{
			ID: 42, Model: models[0].ID, ReasoningLevel: "high",
			Attributes: map[string]any{"manager_id": "telegram-main"},
		}},
	}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/model")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "<b>Model:</b> "+models[0].DisplayName+" (stale catalog) · effort: high")
	assert.Contains(t, messages[0], "\n1. "+models[0].DisplayName+" (stale catalog) · ")
	assert.Contains(t, messages[0], "\n2. local/Plain · free")
	assert.NotContains(t, messages[0], "local/Plain (stale catalog)")
	m.handleChatText(t.Context(), "/model 1")
	require.Len(t, messages, 2)
	assert.Equal(t, "🧠 <b>Model:</b> "+models[0].DisplayName+" (stale catalog) · effort: high", messages[1])
}

func TestModelNumberUsesDefaultAndValidatesEffort(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, listModels: pickerModels}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/model 1")
	require.Equal(t, []controllerapi.SessionSetModelData{{
		SessionID: 42, Model: pickerModels[0].ID, ReasoningLevel: "medium",
	}}, ctrl.setModelCalls)
	m.handleChatText(t.Context(), "/model 1 max")
	assert.Equal(t, "max", ctrl.setModelCalls[1].ReasoningLevel)
	m.handleChatText(t.Context(), "/model 2")
	assert.Empty(t, ctrl.setModelCalls[2].ReasoningLevel)
	m.handleChatText(t.Context(), "/model 1 unsupported")
	assert.Contains(t, messages[len(messages)-1], "Allowed effort levels: low, medium, high, max.")
	m.handleChatText(t.Context(), "/model 2 high")
	assert.Contains(t, messages[len(messages)-1], "Allowed effort levels: none.")
	for _, input := range []string{"/model 0", "/model 3", "/model local/plain", "/model 1 high extra"} {
		m.handleChatText(t.Context(), input)
		assert.Contains(t, messages[len(messages)-1], "Usage: /model")
	}
	assert.Len(t, ctrl.setModelCalls, 3)
}

func TestModelSwitchReportsFailureAndRefetchesAfterRestart(t *testing.T) {
	var messages []string
	ctrl := &fakeController{focus: 42, listModels: pickerModels, setModelErr: errors.New("unavailable")}
	m := newCmdTestManager(t, ctrl, &messages)
	m.handleChatText(t.Context(), "/model 1")
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "Model switch failed: unavailable")
	second := newCmdTestManager(t, ctrl, &messages)
	ctrl.setModelErr = nil
	second.handleChatText(t.Context(), "/model 2")
	assert.Equal(t, 2, ctrl.listModelsCalls)
	assert.Equal(t, pickerModels[1].ID, ctrl.setModelCalls[1].Model)
}

func TestFormatPrice(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{3, "3"},
		{15, "15"},
		{0.5, "0.5"},
		{0.25, "0.25"},
		{3.75, "3.75"},
		{0.125, "0.13"},
		{0.004, "0.004"},
		{0.0001, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, formatPrice(tt.in))
		})
	}
}

func TestModelPriceTag(t *testing.T) {
	assert.Equal(t, "free", modelPriceTag(controllerapi.ConfigModelInfo{}))
	assert.Equal(t, "$0/$2", modelPriceTag(controllerapi.ConfigModelInfo{OutputPrice: 2}))
	assert.Equal(t, "$0.8/$4", modelPriceTag(controllerapi.ConfigModelInfo{InputPrice: 0.8, OutputPrice: 4}))
}

func TestModelDisplayNameFallsBack(t *testing.T) {
	assert.Equal(
		t,
		"p/Name",
		modelDisplayName(controllerapi.ConfigModelInfo{ID: "x", Name: "Name", DisplayName: "p/Name"}),
	)
	assert.Equal(t, "Name", modelDisplayName(controllerapi.ConfigModelInfo{ID: "x", Name: "Name"}))
	assert.Equal(t, "x", modelDisplayName(controllerapi.ConfigModelInfo{ID: "x"}))
}

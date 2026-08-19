package telegram

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

// effortLabels decorates the gateway effort vocabulary. Which of these a model
// actually offers comes from its catalog entry, never from this map.
var effortLabels = map[string]string{
	"none":    "🚫 None",
	"minimal": "· Minimal",
	"low":     "⚡️ Low",
	"medium":  "Medium",
	"high":    "🔥 High",
	"xhigh":   "🔥🔥 X-High",
	"max":     "🧠 Max",
}

func (m *Manager) handleModel(ctx context.Context, sessionID, threadID int64) {
	modelsResult, err := m.controller.ListModels(ctx)
	if err != nil {
		_, _ = m.sendMessage(ctx, "Failed to list models: "+err.Error(), nil, threadID)
		return
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		_, _ = m.sendMessage(ctx, "Failed to list sessions: "+err.Error(), nil, threadID)
		return
	}

	m.availableModels = modelsResult.Models
	if len(m.availableModels) == 0 {
		_, _ = m.sendMessage(ctx, "No models configured. Add models to "+config.DefaultUnifiedConfigFile, nil, threadID)
		return
	}

	currentModel, effort := "", ""

	for _, s := range m.filterOwnedActiveSessions(sessions) {
		if s.ID == sessionID {
			currentModel, effort = s.Model, s.ReasoningLevel
			break
		}
	}

	_, _ = m.sendRawHTML(
		ctx,
		m.modelHeader(ctx, currentModel, effort),
		&tgReplyMarkup{InlineKeyboard: modelKeyboard(m.availableModels, currentModel)},
		threadID,
	)
}

func (m *Manager) handleCallbackModel(
	ctx context.Context,
	cb *telegramCallbackData,
	action callbackAction,
	threadID int64,
) {
	sessionID, ok := m.resolveSessionByTopicID(ctx, threadID)
	if !ok {
		m.answerCallback(ctx, cb.ID, "No session in this context")
		return
	}

	info := m.modelInfo(ctx, action.ModelID)

	// A model switch always resets effort to the new model's own default, so the
	// session never carries a level the new model cannot honour.
	if err := m.controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
		SessionID:      sessionID,
		Model:          action.ModelID,
		ReasoningLevel: info.DefaultEffort,
	}); err != nil {
		m.answerCallback(ctx, cb.ID, "Switch failed: "+err.Error())
		return
	}

	m.answerCallback(ctx, cb.ID, modelDisplayName(info))

	header := m.modelHeader(ctx, action.ModelID, info.DefaultEffort)

	if len(info.EffortLevels) > 0 {
		_ = m.editMessageRawHTML(
			ctx,
			cb.Message.MessageID,
			header+"\n<i>Pick reasoning effort</i>",
			&tgReplyMarkup{InlineKeyboard: effortKeyboard(action.ModelID, info.EffortLevels, info.DefaultEffort)},
		)

		return
	}

	_ = m.editMessageRawHTML(
		ctx,
		cb.Message.MessageID,
		header+"\n<i>Switched for current session</i>",
		&tgReplyMarkup{InlineKeyboard: modelKeyboard(m.availableModels, action.ModelID)},
	)
}

func (m *Manager) handleCallbackEffort(
	ctx context.Context,
	cb *telegramCallbackData,
	action callbackAction,
	threadID int64,
) {
	sessionID, ok := m.resolveSessionByTopicID(ctx, threadID)
	if !ok {
		m.answerCallback(ctx, cb.ID, "No session in this context")
		return
	}

	if err := m.controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
		SessionID:      sessionID,
		Model:          action.ModelID,
		ReasoningLevel: action.Effort,
	}); err != nil {
		m.answerCallback(ctx, cb.ID, "Effort change failed: "+err.Error())
		return
	}

	m.answerCallback(ctx, cb.ID, action.Effort)

	_ = m.editMessageRawHTML(
		ctx,
		cb.Message.MessageID,
		m.modelHeader(ctx, action.ModelID, action.Effort),
		nil,
	)
}

func (m *Manager) modelHeader(ctx context.Context, modelID, effort string) string {
	name := "unknown"
	if modelID != "" {
		name = modelDisplayName(m.modelInfo(ctx, modelID))
	}

	header := "🧠 <b>Model:</b> " + escapeHTML(name)
	if effort == "" {
		return header
	}

	return header + " · effort: " + escapeHTML(effort)
}

// modelInfo resolves a model from the picker cache, refilling it when a callback
// arrives after a manager restart cleared it.
func (m *Manager) modelInfo(ctx context.Context, modelID string) controllerapi.ConfigModelInfo {
	if len(m.availableModels) == 0 {
		if res, err := m.controller.ListModels(ctx); err == nil {
			m.availableModels = res.Models
		}
	}

	for _, model := range m.availableModels {
		if model.ID == modelID {
			return model
		}
	}

	return controllerapi.ConfigModelInfo{ID: modelID}
}

func modelKeyboard(models []controllerapi.ConfigModelInfo, currentID string) [][]tgInlineButton {
	buttons := make([][]tgInlineButton, 0, len(models))

	for _, model := range models {
		label := modelDisplayName(model) + " · " + modelPriceTag(model)
		if model.ID == currentID {
			label = "✅ " + label
		}

		buttons = append(buttons, []tgInlineButton{{
			Text:         label,
			CallbackData: callbackModel + ":" + model.ID,
		}})
	}

	return buttons
}

// effortKeyboard renders one button per level the model accepts, weakest first.
// Telegram wraps a row at ~8 buttons, and no catalog vocabulary reaches that.
func effortKeyboard(modelID string, levels []string, current string) [][]tgInlineButton {
	row := make([]tgInlineButton, 0, len(levels))

	for _, level := range levels {
		label, ok := effortLabels[level]
		if !ok {
			label = level
		}

		if level == current {
			label = "✅ " + label
		}

		row = append(row, tgInlineButton{
			Text:         label,
			CallbackData: callbackEffort + ":" + modelID + ":" + level,
		})
	}

	return [][]tgInlineButton{row}
}

func modelDisplayName(model controllerapi.ConfigModelInfo) string {
	if model.DisplayName != "" {
		return model.DisplayName
	}

	if model.Name != "" {
		return model.Name
	}

	return model.ID
}

func modelPriceTag(model controllerapi.ConfigModelInfo) string {
	if model.InputPrice == 0 && model.OutputPrice == 0 {
		return "free"
	}

	return "$" + formatPrice(model.InputPrice) + "/$" + formatPrice(model.OutputPrice)
}

// formatPrice renders a per-1M price compactly: two decimals with trailing zeros
// stripped, falling to three when two would round a real price away to nothing.
func formatPrice(v float64) string {
	if s := roundHalfUp(v, 2); s != "0" || v == 0 {
		return s
	}

	return roundHalfUp(v, 3)
}

func roundHalfUp(v float64, digits int) string {
	scale := math.Pow(10, float64(digits))
	rounded := math.Floor(v*scale+0.5) / scale

	s := strconv.FormatFloat(rounded, 'f', digits, 64)
	if !strings.Contains(s, ".") {
		return s
	}

	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

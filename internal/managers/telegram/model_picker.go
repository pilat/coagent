package telegram

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func (m *Manager) handleModel(ctx context.Context, sessionID int64, arg string) {
	arg = strings.TrimSpace(arg)

	result, err := m.controller.ListModels(ctx)
	if err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Failed to list models: "+err.Error(), "\n", " "))
		return
	}

	if len(result.Models) == 0 {
		m.reply(
			ctx,
			"/model",
			strings.ReplaceAll("No models configured. Add models to "+config.DefaultUnifiedConfigFile, "\n", " "),
		)

		return
	}

	if arg == "" {
		m.listModels(ctx, sessionID, result.Models)
		return
	}

	parts := strings.Fields(arg)

	number, err := strconv.Atoi(parts[0])
	if err != nil || number < 1 || number > len(result.Models) || len(parts) > 2 {
		m.reply(
			ctx,
			"/model",
			strings.ReplaceAll("Usage: /model N [effort]. Choose a number from /model.", "\n", " "),
		)

		return
	}

	model := result.Models[number-1]

	effort := model.DefaultEffort
	if len(parts) == 2 {
		effort = parts[1]
		if !slices.Contains(model.EffortLevels, effort) {
			allowed := strings.Join(model.EffortLevels, ", ")
			if allowed == "" {
				allowed = "none"
			}

			m.reply(ctx, "/model", strings.ReplaceAll("Allowed effort levels: "+allowed+".", "\n", " "))

			return
		}
	}

	if err := m.controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
		SessionID: sessionID, Model: model.ID, ReasoningLevel: effort,
	}); err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Model switch failed: "+err.Error(), "\n", " "))
		return
	}

	m.replyHTML(ctx, "/model", modelHeader(model, effort))
}

func (m *Manager) listModels(ctx context.Context, sessionID int64, models []controllerapi.ConfigModelInfo) {
	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Failed to list sessions: "+err.Error(), "\n", " "))
		return
	}

	current, effort := "", ""

	for _, session := range m.filterOwnedActiveSessions(sessions) {
		if session.ID == sessionID {
			current, effort = session.Model, session.ReasoningLevel
			break
		}
	}

	currentInfo := controllerapi.ConfigModelInfo{ID: current}
	for _, model := range models {
		if model.ID == current {
			currentInfo = model
			break
		}
	}

	lines := []string{modelHeader(currentInfo, effort), "Use /model N [effort]:"}

	for i, model := range models {
		line := fmt.Sprintf("%d. %s · %s", i+1, escapeHTML(modelDisplayName(model)), modelPriceTag(model))
		if model.DefaultEffort != "" {
			line += " · default effort: " + escapeHTML(model.DefaultEffort)
		}

		if model.ID == current {
			line += " ◀"
		}

		lines = append(lines, line)
	}

	m.replyHTML(ctx, "/model", strings.Join(lines, "\n"))
}

func modelHeader(model controllerapi.ConfigModelInfo, effort string) string {
	name := modelDisplayName(model)
	if name == "" {
		name = "unknown"
	}

	header := "🧠 <b>Model:</b> " + escapeHTML(name)
	if effort == "" {
		return header
	}

	return header + " · effort: " + escapeHTML(effort)
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

package telegram

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/config"
)

func (m *Manager) handleModel(ctx context.Context, sessionID int64, arg string) {
	arg = strings.TrimSpace(arg)

	result, err := m.api.discovery.ListModels(ctx, &coagentv1.ListModelsRequest{})
	if err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Failed to list models: "+err.Error(), "\n", " "))
		return
	}

	if len(result.GetModels()) == 0 {
		m.reply(
			ctx,
			"/model",
			strings.ReplaceAll("No models configured. Add models to "+config.DefaultUnifiedConfigFile, "\n", " "),
		)

		return
	}

	if arg == "" {
		m.listModels(ctx, sessionID, result.GetModels())
		return
	}

	parts := strings.Fields(arg)

	number, err := strconv.Atoi(parts[0])
	if err != nil || number < 1 || number > len(result.GetModels()) || len(parts) > 2 {
		m.reply(
			ctx,
			"/model",
			strings.ReplaceAll("Usage: /model N [effort]. Choose a number from /model.", "\n", " "),
		)

		return
	}

	model := result.GetModels()[number-1]

	effort := model.GetDefaultEffort()
	if len(parts) == 2 {
		effort = parts[1]
		if !slices.Contains(model.GetEffortLevels(), effort) {
			allowed := strings.Join(model.GetEffortLevels(), ", ")
			if allowed == "" {
				allowed = "none"
			}

			m.reply(ctx, "/model", strings.ReplaceAll("Allowed effort levels: "+allowed+".", "\n", " "))

			return
		}
	}

	if _, err := m.api.sessions.SetSessionModel(ctx, &coagentv1.SetSessionModelRequest{
		SessionId: sessionID, Model: model.GetId(), Effort: effort,
	}); err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Model switch failed: "+err.Error(), "\n", " "))
		return
	}

	m.replyHTML(ctx, "/model", modelHeader(model, effort))
}

func (m *Manager) listModels(ctx context.Context, sessionID int64, models []*coagentv1.Model) {
	sessions, err := m.api.sessions.ListSessionDetails(ctx, &coagentv1.ListSessionDetailsRequest{})
	if err != nil {
		m.reply(ctx, "/model", strings.ReplaceAll("Failed to list sessions: "+err.Error(), "\n", " "))
		return
	}

	current, effort := "", ""

	for _, session := range m.filterOwnedActiveSessions(sessions.GetSessions()) {
		if session.GetId() == sessionID {
			current, effort = session.GetModel(), session.GetEffort()
			break
		}
	}

	currentInfo := &coagentv1.Model{Id: current}
	for _, model := range models {
		if model.GetId() == current {
			currentInfo = model
			break
		}
	}

	lines := []string{modelHeader(currentInfo, effort), "Use /model N [effort]:"}

	for i, model := range models {
		line := fmt.Sprintf("%d. %s · %s", i+1, escapeHTML(modelMarkedName(model)), modelPriceTag(model))
		if model.GetDefaultEffort() != "" {
			line += " · default effort: " + escapeHTML(model.GetDefaultEffort())
		}

		if model.GetId() == current {
			line += " ◀"
		}

		lines = append(lines, line)
	}

	m.replyHTML(ctx, "/model", strings.Join(lines, "\n"))
}

func modelHeader(model *coagentv1.Model, effort string) string {
	name := modelMarkedName(model)
	if name == "" {
		name = "unknown"
	}

	header := "🧠 <b>Model:</b> " + escapeHTML(name)
	if effort == "" {
		return header
	}

	return header + " · effort: " + escapeHTML(effort)
}

func modelDisplayName(model *coagentv1.Model) string {
	if model.GetDisplayName() != "" {
		return model.GetDisplayName()
	}

	if model.GetName() != "" {
		return model.GetName()
	}

	return model.GetId()
}

func modelMarkedName(model *coagentv1.Model) string {
	name := modelDisplayName(model)
	if model.GetCatalogStale() {
		name += " (stale catalog)"
	}

	return name
}

func modelPriceTag(model *coagentv1.Model) string {
	if model.GetInputPrice() == 0 && model.GetOutputPrice() == 0 {
		return "free"
	}

	return "$" + formatPrice(model.GetInputPrice()) + "/$" + formatPrice(model.GetOutputPrice())
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

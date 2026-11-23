package telegram

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	commandKill      = "/kill"
	commandStop      = "/stop"
	commandClear     = "/clear"
	commandGWT       = "/gwt"
	noFocusHint      = "No session in focus. /new, /spawn or /switch N."
	sessionListLimit = 30
)

var publishedCommands = []map[string]string{
	{tgKeyCommand: "new", tgKeyDescription: "List projects, or create with /new name"},
	{tgKeyCommand: "spawn", tgKeyDescription: "Create a session with /spawn path"},
	{tgKeyCommand: "sessions", tgKeyDescription: "List sessions, optionally filtered by word"},
	{tgKeyCommand: "switch", tgKeyDescription: "Switch to a session with /switch N"},
	{tgKeyCommand: "title", tgKeyDescription: "Rename the current session with /title text"},
	{tgKeyCommand: "orchestrate", tgKeyDescription: "Let this session manage your sessions: /orchestrate on|off"},
	{tgKeyCommand: "kill", tgKeyDescription: "End the current session, or /kill N"},
	{tgKeyCommand: "stop", tgKeyDescription: "Stop the current run, or /stop N"},
	{tgKeyCommand: "help", tgKeyDescription: "Show commands and current skills"},
	{tgKeyCommand: "start", tgKeyDescription: "Show commands and current skills"},
	{tgKeyCommand: "gwt", tgKeyDescription: "Create a worktree with /gwt name"},
	{tgKeyCommand: "model", tgKeyDescription: "List models, or /model N effort"},
	{tgKeyCommand: "clear", tgKeyDescription: "Clear the current session"},
	{tgKeyCommand: "compact", tgKeyDescription: "Compact the current context"},
	{tgKeyCommand: "status", tgKeyDescription: "Show current session stats"},
	{tgKeyCommand: "budget", tgKeyDescription: "Set a cost or wall-time checkpoint"},
	{tgKeyCommand: "schedules", tgKeyDescription: "List current session schedules"},
	{tgKeyCommand: "config", tgKeyDescription: "Edit the daemon configuration"},
	{tgKeyCommand: "skill", tgKeyDescription: "Use a current session skill"},
}

func normalizeTextCommand(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return text
	}

	parts := strings.SplitN(text, " ", 2)

	cmd := parts[0]
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}

	if len(parts) == 2 {
		return cmd + " " + parts[1]
	}

	return cmd
}

func (m *Manager) handleChatText(ctx context.Context, text string) {
	if name, ok := parseNewCommand(text); ok {
		m.handleNew(ctx, name)
		return
	}

	command, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)

	switch command {
	case "/spawn":
		m.handleSpawn(ctx, arg)
	case "/sessions":
		m.handleSessions(ctx, arg)
	case "/switch":
		id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if err != nil || id <= 0 {
			m.reply(ctx, command, strings.ReplaceAll("Usage: /switch N.", "\n", " "))
			return
		}

		m.switchChat(ctx, command, id)
	case commandKill, commandStop:
		if arg != "" {
			m.handleNumberedControl(ctx, command, arg)
			return
		}

		m.handleFocusedText(ctx, command, text)
	default:
		m.handleFocusedText(ctx, command, text)
	}
}

func (m *Manager) handleFocusedText(ctx context.Context, command, text string) {
	if !strings.HasPrefix(command, "/") {
		command = ""
	}

	focus, err := m.api.chats.GetChatFocus(
		ctx,
		&coagentv1.GetChatFocusRequest{ChatId: strconv.FormatInt(m.effectiveChatID(), 10)},
	)
	if err != nil {
		m.reply(ctx, command, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	_, arg, _ := strings.Cut(text, " ")

	if focus.GetSessionId() == 0 {
		if command == "/help" || command == "/start" {
			m.handleHelp(ctx, command, 0)
		} else {
			m.reply(ctx, command, noFocusHint)
		}

		return
	}

	switch command {
	case "/title":
		m.handleTitle(ctx, focus.GetSessionId(), arg)
	case "/orchestrate":
		m.handleOrchestrate(ctx, focus.GetSessionId(), arg)
	case commandGWT:
		name, _ := parseGwtCommand(text)
		m.handleGWT(ctx, focus.GetSessionId(), name)
	case "/model":
		m.handleModel(ctx, focus.GetSessionId(), strings.TrimSpace(arg))
	case "/help", "/start":
		m.handleHelp(ctx, command, focus.GetSessionId())
	default:
		m.handleSessionMessage(ctx, focus.GetSessionId(), command, text)
	}
}

func (m *Manager) handleTitle(ctx context.Context, focus int64, raw string) {
	if strings.TrimSpace(raw) == "" {
		m.reply(ctx, "/title", strings.ReplaceAll("Usage: /title text.", "\n", " "))
		return
	}

	title, ok := controllerapi.NormalizeSessionTitle(raw)
	if !ok {
		m.reply(ctx, "/title", strings.ReplaceAll("Title must be one line of 1-80 characters.", "\n", " "))
		return
	}

	if _, err := m.api.sessions.SetSessionTitle(
		ctx,
		&coagentv1.SetSessionTitleRequest{SessionId: focus, Title: title},
	); err != nil {
		m.reply(ctx, "/title", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	m.reply(ctx, "/title", strings.ReplaceAll(fmt.Sprintf("Renamed #%d: %s", focus, title), "\n", " "))
}

func (m *Manager) switchChat(ctx context.Context, command string, id int64) {
	m.mu.RLock()
	delivery := m.delivery
	m.mu.RUnlock()

	if _, err := m.api.chats.SetChatFocus(
		ctx,
		&coagentv1.SetChatFocusRequest{ChatId: strconv.FormatInt(m.effectiveChatID(), 10), SessionId: id},
	); err != nil {
		m.reply(ctx, command, strings.ReplaceAll(err.Error(), "\n", " "))

		return
	}

	if delivery != nil {
		delivery.Wake()
	}
}

func (m *Manager) handleNumberedControl(ctx context.Context, command, arg string) {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil {
		m.reply(ctx, command, strings.ReplaceAll("Usage: "+command+" N.", "\n", " "))
		return
	}

	sessions, err := m.api.sessions.ListSessions(ctx, &coagentv1.ListSessionsRequest{})
	if err != nil {
		m.reply(ctx, command, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	for _, session := range sessions.GetSessions() {
		if session.GetId() == id && id > 0 {
			m.handleSessionMessage(ctx, id, command, command)
			return
		}
	}

	m.reply(ctx, command, strings.ReplaceAll(fmt.Sprintf("#%d is not a live session", id), "\n", " "))
}

func (m *Manager) handleHelp(ctx context.Context, command string, sessionID int64) {
	lines := make([]string, 1, 1+len(publishedCommands))
	lines[0] = "<b>Commands:</b>"

	for _, command := range publishedCommands {
		lines = append(lines, "  /"+command[tgKeyCommand]+" — "+escapeHTML(command[tgKeyDescription]))
	}

	m.replyHTML(ctx, command, strings.Join(lines, "\n"))

	if sessionID > 0 {
		m.handleCommands(ctx, command, sessionID)
	}
}

func (m *Manager) handleCommands(ctx context.Context, command string, sessionID int64) {
	result, err := m.api.discovery.ListSkills(ctx, &coagentv1.ListSkillsRequest{SessionId: sessionID})
	if err != nil || len(result.GetSkills()) == 0 {
		m.replyHTML(ctx, command, "No skills loaded.")
		return
	}

	lines := []string{"<b>Skills:</b>"}

	for _, skill := range result.GetSkills() {
		desc := ""
		if skill.GetDescription() != "" {
			desc = " — " + escapeHTML(skill.GetDescription())
		}

		lines = append(lines, "  /skill "+escapeHTML(skill.GetName())+desc)
	}

	m.replyHTML(ctx, command, strings.Join(lines, "\n"))
}

func (m *Manager) handleSessionMessage(ctx context.Context, sessionID int64, command, text string) {
	if _, err := m.api.sessions.SendInput(ctx, &coagentv1.SendInputRequest{
		SessionId: sessionID, Text: text, IdempotencyKey: rand.Text(),
	}); err != nil {
		m.reply(ctx, command, strings.ReplaceAll(err.Error(), "\n", " "))
	}
}

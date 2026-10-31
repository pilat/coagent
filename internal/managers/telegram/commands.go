package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	commandKill      = "/kill"
	commandStop      = "/stop"
	commandClear     = "/clear"
	commandGWT       = "/gwt"
	telegramChannel  = "telegram"
	sessionListLimit = 30
)

var publishedCommands = []map[string]string{
	{tgKeyCommand: "new", tgKeyDescription: "List projects, or create with /new name"},
	{tgKeyCommand: "spawn", tgKeyDescription: "Create a session with /spawn path"},
	{tgKeyCommand: "sessions", tgKeyDescription: "List sessions and unread output"},
	{tgKeyCommand: "switch", tgKeyDescription: "Switch to a session with /switch N"},
	{tgKeyCommand: "title", tgKeyDescription: "Rename the current session with /title text"},
	{tgKeyCommand: "assistant", tgKeyDescription: "Switch to Assistant"},
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
		m.handleSessions(ctx)
	case "/switch":
		id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if err != nil {
			_, _ = m.sendMessage(ctx, strings.ReplaceAll("Usage: /switch N.", "\n", " "))
			return
		}

		m.switchChat(ctx, id)
	case "/assistant":
		m.switchChat(ctx, 0)
	case commandKill, commandStop:
		if arg != "" {
			m.handleNumberedControl(ctx, command, arg)
			return
		}

		m.handleFocusedText(ctx, text)
	default:
		m.handleFocusedText(ctx, text)
	}
}

func (m *Manager) handleFocusedText(ctx context.Context, text string) {
	focus, err := m.chat.ChatFocus(ctx, strconv.FormatInt(m.effectiveChatID(), 10))
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	command, arg, _ := strings.Cut(text, " ")
	if focus != 0 && command == "/config" {
		_, _ = m.sendMessage(
			ctx,
			strings.ReplaceAll("/config works on the assistant: /assistant, then /config", "\n", " "),
		)

		return
	}

	if focus == 0 {
		switch command {
		case commandKill, commandStop, commandClear, "/compact", "/title":
			_, _ = m.sendMessage(ctx, strings.ReplaceAll("This command is unavailable for Assistant.", "\n", " "))
			return
		case commandGWT:
			_, _ = m.sendMessage(ctx, strings.ReplaceAll("Switch to a work session before using /gwt.", "\n", " "))
			return
		}
	}

	sessionID, err := m.inputSessionID(ctx, focus)
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	switch command {
	case "/title":
		m.handleTitle(ctx, focus, arg)
	case commandGWT:
		name, _ := parseGwtCommand(text)
		m.handleGWT(ctx, sessionID, name)
	case "/model":
		m.handleModel(ctx, sessionID, strings.TrimSpace(arg))
	case "/help", "/start":
		m.handleHelp(ctx, sessionID)
	default:
		m.handleSessionMessage(ctx, sessionID, text)
	}
}

func (m *Manager) handleTitle(ctx context.Context, focus int64, raw string) {
	if strings.TrimSpace(raw) == "" {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll("Usage: /title text.", "\n", " "))
		return
	}

	title, ok := controllerapi.NormalizeSessionTitle(raw)
	if !ok {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll("Title must be one line of 1-80 characters.", "\n", " "))
		return
	}

	if err := m.chat.SetSessionTitle(ctx, strconv.FormatInt(m.effectiveChatID(), 10), focus, title); err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	_, _ = m.sendMessage(ctx, strings.ReplaceAll(fmt.Sprintf("Renamed #%d: %s", focus, title), "\n", " "))
}

func (m *Manager) inputSessionID(ctx context.Context, focus int64) (int64, error) {
	if focus != 0 {
		return focus, nil
	}

	m.mu.RLock()
	id := m.assistantRootID
	m.mu.RUnlock()

	if id != 0 {
		return id, nil
	}

	root, err := m.controller.EnsureAssistantRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("ensure assistant input: %w", err)
	}

	m.mu.Lock()
	m.assistantRootID = root.SessionID
	m.mu.Unlock()

	return root.SessionID, nil
}

func (m *Manager) switchChat(ctx context.Context, id int64) {
	m.mu.RLock()
	delivery := m.delivery
	assistantID := m.assistantRootID
	m.mu.RUnlock()

	target := id
	if id == assistantID {
		target = 0
	}

	if delivery != nil {
		delivery.Enter(target)
	}

	if err := m.chat.SetChatFocus(ctx, strconv.FormatInt(m.effectiveChatID(), 10), id); err != nil {
		if delivery != nil {
			delivery.Enter(-1)
		}

		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))

		return
	}

	if delivery != nil {
		delivery.Wake()
	}
}

func (m *Manager) handleNumberedControl(ctx context.Context, command, arg string) {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll("Usage: "+command+" N.", "\n", " "))
		return
	}

	sessions, err := m.chat.ChatSessions(ctx, strconv.FormatInt(m.effectiveChatID(), 10))
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	for _, session := range sessions {
		if session.ID == id && id != 0 {
			m.handleSessionMessage(ctx, id, command)
			return
		}
	}

	_, _ = m.sendMessage(ctx, strings.ReplaceAll(fmt.Sprintf("#%d is not a live session", id), "\n", " "))
}

func (m *Manager) handleSessions(ctx context.Context) {
	sessions, err := m.chat.ChatSessions(ctx, strconv.FormatInt(m.effectiveChatID(), 10))
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	focus, err := m.chat.ChatFocus(ctx, strconv.FormatInt(m.effectiveChatID(), 10))
	if err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	lines := make([]string, 0, min(len(sessions), sessionListLimit)+1)
	for _, session := range sessions[:min(len(sessions), sessionListLimit)] {
		line := fmt.Sprintf("#%d %s", session.ID, session.Name)
		if session.Scheduled {
			line += " ⏰"
		}

		if session.Unread > 0 {
			line += fmt.Sprintf(" · %d new", session.Unread)
		}

		if session.ID == focus {
			line += " ◀"
		}

		lines = append(lines, line)
	}

	if remaining := len(sessions) - sessionListLimit; remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more — ask the assistant to find one", remaining))
	}

	_, _ = m.sendMessage(ctx, strings.Join(lines, "\n"))
}

func (m *Manager) handleHelp(ctx context.Context, sessionID int64) {
	lines := make([]string, 1, 1+len(publishedCommands))
	lines[0] = "<b>Commands:</b>"

	for _, command := range publishedCommands {
		lines = append(lines, "  /"+command[tgKeyCommand]+" — "+escapeHTML(command[tgKeyDescription]))
	}

	_, _ = m.sendRawHTML(ctx, strings.Join(lines, "\n"))
	m.handleCommands(ctx, sessionID)
}

func (m *Manager) handleCommands(ctx context.Context, sessionID int64) {
	result, err := m.controller.ListSkills(ctx, controllerapi.ConfigSkillsData{SessionID: sessionID})
	if err != nil || len(result.Skills) == 0 {
		_, _ = m.sendRawHTML(ctx, "No skills loaded.")
		return
	}

	lines := []string{"<b>Skills:</b>"}

	for _, skill := range result.Skills {
		desc := ""
		if skill.Description != "" {
			desc = " — " + escapeHTML(skill.Description)
		}

		lines = append(lines, "  /skill "+escapeHTML(skill.Name)+desc)
	}

	_, _ = m.sendRawHTML(ctx, strings.Join(lines, "\n"))
}

func (m *Manager) handleSessionMessage(ctx context.Context, sessionID int64, text string) {
	if err := m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
		SessionID: sessionID, Message: text,
	}); err != nil {
		_, _ = m.sendMessage(ctx, strings.ReplaceAll(err.Error(), "\n", " "))
	}
}

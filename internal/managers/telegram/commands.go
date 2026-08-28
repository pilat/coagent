package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	callbackNav       = "nav"
	callbackLaunch    = "launch"
	callbackLaunchGWT = "launch_gwt"
	callbackMore      = "more"
	callbackSpawn     = "spawn"
	callbackKill      = "kill"
	callbackModel     = "model"
	callbackEffort    = "effort"
	callbackNewPick   = "newpick"
	callbackNewPage   = "newpage"
	commandKill       = "/kill"
	commandStop       = "/stop"
	telegramChannel   = "telegram"
)

type callbackAction struct {
	Kind      string
	DirID     int64
	Offset    int
	Session   int64
	ModelID   string
	Effort    string
	ProjectID int64
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

func (m *Manager) handleServiceTopicMessage(ctx context.Context, text string) {
	// /new takes an argument, so it needs prefix dispatch — every other service
	// command is exact-match.
	if name, ok := parseNewCommand(text); ok {
		m.handleNew(ctx, name)
		return
	}

	switch text {
	case "/start":
		_, _ = m.sendMessage(ctx, "Use /spawn to create a session, or /kill to stop one.", nil, m.serviceTopicID)
	case "/spawn":
		m.handleSpawn(ctx, "", 0, 0)
	case commandKill:
		m.handleKill(ctx, 0, m.serviceTopicID)
	case commandStop:
		_, _ = m.sendMessage(ctx, "Use /stop inside a session topic.", nil, m.serviceTopicID)
	case "/model":
		_, _ = m.sendMessage(ctx, "Use /model inside a session topic.", nil, m.serviceTopicID)
	case "/schedules":
		_, _ = m.sendMessage(ctx, "Use /schedules inside a session topic.", nil, m.serviceTopicID)
	case "/help":
		m.handleHelp(ctx, 0, m.serviceTopicID)
	default:
		_, _ = m.sendMessage(
			ctx,
			"Send messages in a session topic, or use /spawn to create one.",
			nil,
			m.serviceTopicID,
		)
	}
}

func (m *Manager) handleSessionTopicMessage(ctx context.Context, sessionID, threadID int64, text string) {
	// /new belongs to the service topic; steer it there instead of to the LLM.
	if _, ok := parseNewCommand(text); ok {
		_, _ = m.sendMessage(ctx, "Use this command in the service topic.", nil, threadID)
		return
	}

	switch text {
	case commandKill:
		m.handleKill(ctx, sessionID, threadID)
	case commandStop:
		_ = m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
			SessionID: sessionID, Message: commandStop,
		})
	case "/clear":
		_ = m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
			SessionID: sessionID, Message: "/clear",
		})
	case "/model":
		m.handleModel(ctx, sessionID, threadID)
	case "/schedules":
		m.handleSessionMessage(ctx, sessionID, text, threadID)
	case "/help":
		m.handleSessionMessage(ctx, sessionID, text, threadID)
	case "/spawn", "/start":
		_, _ = m.sendMessage(ctx, "Use this command in the service topic.", nil, threadID)
	default:
		m.handleSessionMessage(ctx, sessionID, text, threadID)
	}
}

func (m *Manager) handleSpawn(ctx context.Context, dir string, offset int, messageID int64) {
	if messageID == 0 {
		m.resetNavigation()
	}

	result, err := m.controller.ListDir(ctx, controllerapi.FsListDirData{Path: dir})
	if err != nil {
		msg := "Failed to list directories: " + err.Error()
		if messageID > 0 {
			_ = m.editMessageText(ctx, messageID, msg, nil)
		} else {
			_, _ = m.sendMessage(ctx, msg, nil, m.serviceTopicID)
		}

		return
	}

	if result.Home != "" {
		m.daemonHome = result.Home
	}

	if dir == "" && result.Path != "" {
		dir = result.Path
	}

	if dir == "" {
		m.handleSpawnFavorites(ctx, result, messageID)
		return
	}

	m.handleSpawnDir(ctx, dir, offset, result, messageID)
}

func (m *Manager) handleSpawnFavorites(
	ctx context.Context,
	result *controllerapi.FsListDirResultData,
	messageID int64,
) {
	buttons := make([][]tgInlineButton, 0, len(result.Favorites)+len(result.Dirs))
	for _, f := range result.Favorites {
		buttons = append(buttons, []tgInlineButton{{
			Text:         "⭐ " + strings.Replace(f, m.daemonHome, "~", 1),
			CallbackData: "nav:" + strconv.FormatInt(m.navID(f), 10),
		}})
	}

	for _, d := range result.Dirs {
		buttons = append(buttons, []tgInlineButton{{
			Text:         "📁 " + d.Name,
			CallbackData: "nav:" + strconv.FormatInt(m.navID(d.Path), 10),
		}})
	}

	if len(buttons) == 0 {
		if messageID > 0 {
			_ = m.editMessageText(ctx, messageID, "No directories available.", nil)
		} else {
			_, _ = m.sendMessage(ctx, "No directories available.", nil, m.serviceTopicID)
		}

		return
	}

	markup := &tgReplyMarkup{InlineKeyboard: buttons}
	if messageID > 0 {
		_ = m.editMessageText(ctx, messageID, "Pick a folder:", markup)
		return
	}

	_, _ = m.sendMessage(ctx, "Pick a folder:", markup, m.serviceTopicID)
}

func (m *Manager) handleSpawnDir(
	ctx context.Context,
	dir string,
	offset int,
	result *controllerapi.FsListDirResultData,
	messageID int64,
) {
	if offset < 0 {
		offset = 0
	}

	total := len(result.Dirs)
	if offset > total {
		offset = 0
	}

	start, end, hasMore := dirPageBounds(total, offset)
	keyboard := m.buildSpawnDirKeyboard(dir, offset, result, start, end, hasMore)

	dirLabel := strings.Replace(dir, m.daemonHome, "~", 1)

	pageInfo := ""
	if total > foldersPerPage {
		pageInfo = fmt.Sprintf(" (%d-%d of %d)", start+1, end, total)
	}

	text := "📂 " + dirLabel + pageInfo
	markup := &tgReplyMarkup{InlineKeyboard: keyboard}

	if messageID > 0 {
		_ = m.editMessageText(ctx, messageID, text, markup)
		return
	}

	_, _ = m.sendMessage(ctx, text, markup, m.serviceTopicID)
}

func (m *Manager) buildSpawnDirKeyboard(
	dir string,
	offset int,
	result *controllerapi.FsListDirResultData,
	start, end int,
	hasMore bool,
) [][]tgInlineButton {
	keyboard := make([][]tgInlineButton, 0, foldersPerPage+3)

	for _, d := range result.Dirs[start:end] {
		keyboard = append(keyboard, []tgInlineButton{{
			Text:         "📁 " + d.Name,
			CallbackData: "nav:" + strconv.FormatInt(m.navID(d.Path), 10),
		}})
	}

	parent := result.Parent
	if parent == "" {
		parent = filepath.Dir(dir)
	}

	navRow := make([]tgInlineButton, 0, 2)

	if parent != "" && parent != dir {
		navRow = append(navRow, tgInlineButton{
			Text:         "⬆️ ..",
			CallbackData: "nav:" + strconv.FormatInt(m.navID(parent), 10),
		})
	}

	navRow = append(navRow, tgInlineButton{Text: "⭐ Favorites", CallbackData: callbackSpawn})

	launchRow := []tgInlineButton{
		{Text: "🚀 Launch here", CallbackData: "launch:" + strconv.FormatInt(m.navID(dir), 10)},
		{Text: "🌿 GWT", CallbackData: "launch_gwt:" + strconv.FormatInt(m.navID(dir), 10)},
	}

	pagination := make([]tgInlineButton, 0, 2)

	if offset > 0 {
		pagination = append(pagination, tgInlineButton{
			Text:         "⬅️ Back",
			CallbackData: "more:" + strconv.FormatInt(m.navID(dir), 10) + ":" + strconv.Itoa(offset-foldersPerPage),
		})
	}

	if hasMore {
		pagination = append(pagination, tgInlineButton{
			Text:         fmt.Sprintf("➡️ More (%d/%d)", offset+foldersPerPage, len(result.Dirs)),
			CallbackData: "more:" + strconv.FormatInt(m.navID(dir), 10) + ":" + strconv.Itoa(offset+foldersPerPage),
		})
	}

	if len(pagination) > 0 {
		keyboard = append(keyboard, pagination)
	}

	return append(keyboard, navRow, launchRow)
}

func (m *Manager) handleKill(ctx context.Context, sessionID, threadID int64) {
	if sessionID > 0 {
		if err := m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
			SessionID: sessionID, Message: commandKill,
		}); err != nil {
			_, _ = m.sendMessage(ctx, "❌ Kill failed: "+err.Error(), nil, threadID)
		}

		return
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		_, _ = m.sendMessage(ctx, "Failed to list sessions: "+err.Error(), nil, m.serviceTopicID)
		return
	}

	sessions = m.filterOwnedActiveSessions(sessions)
	if len(sessions) == 0 {
		_, _ = m.sendMessage(ctx, "No sessions to kill.", nil, m.serviceTopicID)
		return
	}

	labels := makeLabels(sessions)
	buttons := make([][]tgInlineButton, 0, len(sessions))

	for _, s := range sessions {
		label := s.Name
		if label == "" {
			label = labels[s.ID]
		}

		buttons = append(buttons, []tgInlineButton{{
			Text:         "💀 " + label,
			CallbackData: "kill:" + strconv.FormatInt(s.ID, 10),
		}})
	}

	_, _ = m.sendMessage(ctx, "Kill which session?", &tgReplyMarkup{InlineKeyboard: buttons}, m.serviceTopicID)
}

// displayDir collapses the daemon home to ~ for display. Guards the empty-home
// case: /new can be the first command ever, before any ListDir has populated
// daemonHome, and Replace(dir, "", "~", 1) would prepend a stray ~.
func (m *Manager) displayDir(dir string) string {
	if m.daemonHome == "" {
		return dir
	}

	return strings.Replace(dir, m.daemonHome, "~", 1)
}

func (m *Manager) handleLaunch(ctx context.Context, dir string) {
	_, _ = m.sendMessage(ctx, "🚀 Creating session in "+m.displayDir(dir)+"...", nil, m.serviceTopicID)

	_, err := m.controller.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: dir,
		Attributes: map[string]any{
			"channel": telegramChannel,
		},
	})
	if err != nil {
		_, _ = m.sendMessage(ctx, "❌ Session create failed: "+err.Error(), nil, m.serviceTopicID)
	}
}

func (m *Manager) handleLaunchGWT(ctx context.Context, dir string) {
	_, _ = m.sendMessage(ctx, "🌿 Creating worktree session in "+m.displayDir(dir)+"...", nil, m.serviceTopicID)

	_, err := m.controller.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir:     dir,
		UseWorktree: true,
		Attributes: map[string]any{
			"channel": telegramChannel,
		},
	})
	if err != nil {
		_, _ = m.sendMessage(ctx, "❌ Session create failed: "+err.Error(), nil, m.serviceTopicID)
	}
}

func (m *Manager) handleHelp(ctx context.Context, sessionID, threadID int64) {
	lines := []string{
		"<b>Commands:</b>",
		"  /new — new dialog project by name (/new &lt;name&gt;), or bare /new to pick one",
		"  /spawn — open folder picker for new session",
		"  /kill — end this session (terminal)",
		"  /stop — stop the current run (session stays, resumable)",
		"  /clear — clear session (fresh start, same topic)",
		"  /compact — compact context now; /compact &lt;focus&gt; to steer the summary",
		"  /model — choose LLM model",
		"  /status — show session stats (tokens, cost, context)",
		"  /schedules — list this session's schedules (ask me to add/change them)",
		"  /budget &lt;request&gt; — arm or clear a one-shot cost/wall-time checkpoint",
		"  /help — this message",
	}

	if sessionID == 0 {
		lines = append(
			lines,
			"\n<i>/status, /stop, /clear, /compact, /model, /schedules, /budget work inside a session topic only.</i>",
		)
	}

	_, _ = m.sendRawHTML(ctx, strings.Join(lines, "\n"), nil, threadID)

	if sessionID > 0 {
		m.handleCommands(ctx, sessionID, threadID)
	}
}

func (m *Manager) handleCommands(ctx context.Context, sessionID, threadID int64) {
	result, err := m.controller.ListSkills(ctx, controllerapi.ConfigSkillsData{SessionID: sessionID})
	if err != nil {
		_, _ = m.sendRawHTML(ctx, "No skills loaded.", nil, threadID)
		return
	}

	m.availableSkills = result.Skills
	if len(m.availableSkills) == 0 {
		_, _ = m.sendRawHTML(ctx, "No skills loaded.", nil, threadID)
		return
	}

	lines := []string{"<b>Skills:</b>"}

	for _, skill := range m.availableSkills {
		desc := ""
		if skill.Description != "" {
			desc = " — " + escapeHTML(skill.Description)
		}

		lines = append(lines, "  /skill "+escapeHTML(skill.Name)+desc)
	}

	_, _ = m.sendRawHTML(ctx, strings.Join(lines, "\n"), nil, threadID)
}

func (m *Manager) handleSessionMessage(ctx context.Context, sessionID int64, text string, threadID int64) {
	if err := m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
		SessionID: sessionID,
		Message:   text,
	}); err != nil {
		_, _ = m.sendMessage(ctx, "❌ Failed to send message: "+err.Error(), nil, threadID)
	}
}

func (m *Manager) handleCallback(ctx context.Context, cb *telegramCallbackData) {
	if cb == nil || cb.Message == nil || cb.From == nil {
		return
	}

	if cb.Message.Chat.ID != m.effectiveChatID() {
		return
	}

	if !m.isAllowedUser(cb.From.ID) {
		return
	}

	threadID := cb.Message.MessageThreadID
	if threadID != 0 && threadID != m.serviceTopicID {
		if _, ok := m.resolveSessionByTopicID(ctx, threadID); !ok {
			m.answerCallback(ctx, cb.ID, "")
			return
		}
	}

	action, ok := parseCallbackData(cb.Data)
	if !ok {
		m.answerCallback(ctx, cb.ID, "")
		return
	}

	switch action.Kind {
	case callbackNav:
		m.handleCallbackNav(ctx, cb, action)
	case callbackLaunchGWT:
		m.handleCallbackLaunchGWT(ctx, cb, action)
	case callbackLaunch:
		m.handleCallbackLaunch(ctx, cb, action)
	case callbackMore:
		m.handleCallbackMore(ctx, cb, action)
	case callbackSpawn:
		m.answerCallback(ctx, cb.ID, "")
		m.handleSpawn(ctx, "", 0, cb.Message.MessageID)
	case callbackKill:
		m.handleCallbackKill(ctx, cb, action)
	case callbackModel:
		m.handleCallbackModel(ctx, cb, action, threadID)
	case callbackEffort:
		m.handleCallbackEffort(ctx, cb, action, threadID)
	case callbackNewPick:
		m.handleCallbackNewPick(ctx, cb, action)
	case callbackNewPage:
		m.handleCallbackNewPage(ctx, cb, action)
	}
}

func (m *Manager) handleCallbackNav(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	dir, ok := m.pathByNavID(action.DirID)
	m.answerCallback(ctx, cb.ID, "")

	if ok {
		m.handleSpawn(ctx, dir, 0, cb.Message.MessageID)
	}
}

func (m *Manager) handleCallbackLaunchGWT(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	dir, ok := m.pathByNavID(action.DirID)
	m.answerCallback(ctx, cb.ID, "Creating worktree...")

	if ok {
		m.handleLaunchGWT(ctx, dir)
	}
}

func (m *Manager) handleCallbackLaunch(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	dir, ok := m.pathByNavID(action.DirID)
	m.answerCallback(ctx, cb.ID, "Creating...")

	if ok {
		m.handleLaunch(ctx, dir)
	}
}

func (m *Manager) handleCallbackMore(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	dir, ok := m.pathByNavID(action.DirID)
	m.answerCallback(ctx, cb.ID, "")

	if ok {
		m.handleSpawn(ctx, dir, action.Offset, cb.Message.MessageID)
	}
}

func (m *Manager) handleCallbackKill(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	if !m.ownsActiveSessionID(ctx, action.Session) {
		m.answerCallback(ctx, cb.ID, "Session is no longer available")
		return
	}

	m.answerCallback(ctx, cb.ID, "Killing...")

	if err := m.controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
		SessionID: action.Session,
		Message:   commandKill,
	}); err != nil {
		_, _ = m.sendMessage(ctx, "❌ Kill failed: "+err.Error(), nil, m.serviceTopicID)
	}
}

func (m *Manager) resetNavigation() {
	m.mu.Lock()
	m.navCounter = 0
	m.navPaths = make(map[int64]string)
	m.pathToNav = make(map[string]int64)
	m.mu.Unlock()
}

func (m *Manager) navID(path string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.pathToNav[path]; ok {
		return id
	}

	id := m.navCounter
	m.navCounter++
	m.navPaths[id] = path
	m.pathToNav[path] = id

	return id
}

func (m *Manager) pathByNavID(id int64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	path, ok := m.navPaths[id]

	return path, ok
}

func parseCallbackData(data string) (callbackAction, bool) {
	if id, ok := cutInt64(data, "nav:"); ok {
		return callbackAction{Kind: callbackNav, DirID: id}, true
	}

	if id, ok := cutInt64(data, "launch_gwt:"); ok {
		return callbackAction{Kind: callbackLaunchGWT, DirID: id}, true
	}

	if id, ok := cutInt64(data, "launch:"); ok {
		return callbackAction{Kind: callbackLaunch, DirID: id}, true
	}

	if id, ok := cutInt64(data, "newpick:"); ok {
		return callbackAction{Kind: callbackNewPick, ProjectID: id}, true
	}

	if id, ok := cutInt64(data, "kill:"); ok {
		return callbackAction{Kind: callbackKill, Session: id}, true
	}

	if after, ok := strings.CutPrefix(data, "more:"); ok {
		return parseMoreCallback(after)
	}

	if after, ok := strings.CutPrefix(data, "newpage:"); ok {
		offset, err := strconv.Atoi(after)
		if err != nil {
			return callbackAction{}, false
		}

		return callbackAction{Kind: callbackNewPage, Offset: offset}, true
	}

	if data == "spawn" {
		return callbackAction{Kind: callbackSpawn}, true
	}

	if after, ok := strings.CutPrefix(data, "model:"); ok {
		return callbackAction{Kind: callbackModel, ModelID: after}, true
	}

	if after, ok := strings.CutPrefix(data, "effort:"); ok {
		return parseEffortCallback(after)
	}

	return callbackAction{}, false
}

// parseEffortCallback splits at the LAST colon: model ids legally contain colons
// (Ollama-style "qwen3:14b"), so only the level is delimited.
func parseEffortCallback(after string) (callbackAction, bool) {
	sep := strings.LastIndexByte(after, ':')
	if sep <= 0 || sep == len(after)-1 {
		return callbackAction{}, false
	}

	level := after[sep+1:]
	if _, known := effortLabels[level]; !known {
		return callbackAction{}, false
	}

	return callbackAction{Kind: callbackEffort, ModelID: after[:sep], Effort: level}, true
}

func parseMoreCallback(after string) (callbackAction, bool) {
	parts := strings.Split(after, ":")
	if len(parts) != 2 {
		return callbackAction{}, false
	}

	dirID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return callbackAction{}, false
	}

	offset, err := strconv.Atoi(parts[1])
	if err != nil {
		return callbackAction{}, false
	}

	return callbackAction{Kind: callbackMore, DirID: dirID, Offset: offset}, true
}

// cutInt64 matches a "prefix<int64>" callback and parses the integer. A matched
// prefix with an unparseable tail reports false, same as no match — the caller
// falls through to the next form and ultimately rejects the callback.
func cutInt64(data, prefix string) (int64, bool) {
	after, ok := strings.CutPrefix(data, prefix)
	if !ok {
		return 0, false
	}

	id, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return 0, false
	}

	return id, true
}

func dirPageBounds(total, offset int) (int, int, bool) {
	if offset < 0 {
		offset = 0
	}

	if offset > total {
		offset = 0
	}

	start := offset

	end := min(offset+foldersPerPage, total)

	hasMore := end < total

	return start, end, hasMore
}

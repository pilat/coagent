package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	tgKeyCommand     = "command"
	tgKeyDescription = "description"
	tgKeyChatID      = "chat_id"
	tgKeyText        = "text"
	tgKeyMessageID   = "message_id"
	tgKeyParseMode   = "parse_mode"
	tgKeyLinkPreview = "link_preview_options"

	tgParseModeHTML = "HTML"
)

type tgAPIResponse struct {
	OK          bool                  `json:"ok"`
	Result      json.RawMessage       `json:"result"`
	Description string                `json:"description"`
	ErrorCode   int                   `json:"error_code"`
	Parameters  *tgResponseParameters `json:"parameters"`
}

type tgResponseParameters struct {
	RetryAfter int `json:"retry_after"`
}

type tgAPIError struct {
	Method      string
	Description string
	ErrorCode   int
	RetryAfter  int // seconds; from a 429's parameters.retry_after, 0 when absent
}

func (e *tgAPIError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("telegram %s failed (code %d)", e.Method, e.ErrorCode)
	}

	return fmt.Sprintf("telegram %s failed (code %d): %s", e.Method, e.ErrorCode, e.Description)
}

func (m *Manager) tg(ctx context.Context, method string, params map[string]any, out any) error {
	disableLinkPreview(method, params)

	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}

	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/%s", m.cfg.BotToken, method)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", sanitizeTransportError(err))
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call telegram %s: %w", method, sanitizeTransportError(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	var parsed tgAPIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse telegram response: %w", err)
	}

	if !parsed.OK {
		apiErr := &tgAPIError{Method: method, Description: parsed.Description, ErrorCode: parsed.ErrorCode}
		if parsed.Parameters != nil {
			apiErr.RetryAfter = parsed.Parameters.RetryAfter
		}

		return apiErr
	}

	if out == nil {
		return nil
	}

	if len(parsed.Result) == 0 || string(parsed.Result) == "null" {
		return nil
	}

	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("parse telegram result: %w", err)
	}

	return nil
}

// sanitizeTransportError drops the *url.Error wrapper, whose text embeds the
// full request URL — for telegram that URL carries the bot token.
func sanitizeTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}

// disableLinkPreview suppresses URL preview cards on outgoing messages unless the caller set the option.
func disableLinkPreview(method string, params map[string]any) {
	if method != "sendMessage" && method != "editMessageText" {
		return
	}

	if _, ok := params[tgKeyLinkPreview]; ok {
		return
	}

	params[tgKeyLinkPreview] = map[string]any{"is_disabled": true}
}

func (m *Manager) getUpdates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	var updates []telegramUpdate
	if err := m.tg(ctx, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": m.cfg.PollTimeoutSec,
		"allowed_updates": []string{
			"message",
			"callback_query",
		},
	}, &updates); err != nil {
		return nil, err
	}

	return updates, nil
}

func (m *Manager) setCommands(ctx context.Context) error {
	return m.tg(ctx, "setMyCommands", map[string]any{
		"commands": []map[string]string{
			{tgKeyCommand: "new", tgKeyDescription: "New dialog project by name, or pick one"},
			{tgKeyCommand: "spawn", tgKeyDescription: "Open folder picker for new session"},
			{tgKeyCommand: "kill", tgKeyDescription: "End this session (terminal)"},
			{tgKeyCommand: "stop", tgKeyDescription: "Stop the current run (keep session)"},
			{tgKeyCommand: "clear", tgKeyDescription: "Clear session (fresh start, same topic)"},
			{tgKeyCommand: "model", tgKeyDescription: "Choose LLM model"},
			{tgKeyCommand: "status", tgKeyDescription: "Show session stats (tokens, cost, context)"},
			{tgKeyCommand: "help", tgKeyDescription: "Show available commands"},
		},
	}, nil)
}

func (m *Manager) answerCallback(ctx context.Context, callbackID, text string) {
	params := map[string]any{
		"callback_query_id": callbackID,
	}
	if text != "" {
		params[tgKeyText] = text
	}

	_ = m.tg(ctx, "answerCallbackQuery", params, nil)
}

func (m *Manager) sendTyping(ctx context.Context, threadID int64) error {
	params := map[string]any{
		tgKeyChatID: m.cfg.TargetChatID,
		"action":    "typing",
	}
	if threadID > 0 {
		params["message_thread_id"] = threadID
	}

	return m.tg(ctx, "sendChatAction", params, nil)
}

func (m *Manager) sendMessage(ctx context.Context, text string, markup *tgReplyMarkup, threadID int64) (int64, error) {
	fullHTML := textToTelegramHTML(text)
	chunks := splitMessageChunks(fullHTML, maxMessageChunk)

	var lastID int64

	for i, chunk := range chunks {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(m.cfg.SendChunkDelayMS) * time.Millisecond):
			}
		}

		id, err := m.sendMessageChunk(ctx, chunk, markup, threadID)
		if err != nil {
			if !shouldFallbackToPlain(err) {
				return 0, err
			}

			plainID, plainErr := m.sendPlainFallback(ctx, chunk, markup, threadID)
			if plainErr != nil {
				return 0, err
			}

			lastID = plainID

			continue
		}

		lastID = id
	}

	return lastID, nil
}

// shouldFallbackToPlain is deliberately narrow. Retrying an ambiguous transport
// or server failure as a second sendMessage can duplicate a message Telegram
// already accepted. Only an explicit 400 entity-parse rejection proves the HTML
// request was not accepted and that removing parse_mode can make it valid.
func shouldFallbackToPlain(err error) bool {
	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != http.StatusBadRequest {
		return false
	}

	return strings.Contains(strings.ToLower(apiErr.Description), "parse entities")
}

func (m *Manager) sendRawHTML(ctx context.Context, html string, markup *tgReplyMarkup, threadID int64) (int64, error) {
	params := map[string]any{
		tgKeyChatID:    m.cfg.TargetChatID,
		tgKeyText:      html,
		tgKeyParseMode: tgParseModeHTML,
	}
	if markup != nil {
		params["reply_markup"] = markup
	}

	if threadID > 0 {
		params["message_thread_id"] = threadID
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

func (m *Manager) editMessageText(ctx context.Context, messageID int64, text string, markup *tgReplyMarkup) error {
	params := map[string]any{
		tgKeyChatID:    m.cfg.TargetChatID,
		tgKeyMessageID: messageID,
		tgKeyText:      textToTelegramHTML(text),
		tgKeyParseMode: tgParseModeHTML,
	}
	if markup != nil {
		params["reply_markup"] = markup
	}

	return m.tg(ctx, "editMessageText", params, nil)
}

func (m *Manager) editMessageRawHTML(ctx context.Context, messageID int64, html string, markup *tgReplyMarkup) error {
	params := map[string]any{
		tgKeyChatID:    m.cfg.TargetChatID,
		tgKeyMessageID: messageID,
		tgKeyText:      html,
		tgKeyParseMode: tgParseModeHTML,
	}
	if markup != nil {
		params["reply_markup"] = markup
	}

	return m.tg(ctx, "editMessageText", params, nil)
}

func (m *Manager) deleteMessage(ctx context.Context, messageID int64) error {
	return m.tg(ctx, "deleteMessage", map[string]any{
		tgKeyChatID:    m.cfg.TargetChatID,
		tgKeyMessageID: messageID,
	}, nil)
}

func (m *Manager) createForumTopic(ctx context.Context, name, emojiID string) (int64, error) {
	params := map[string]any{
		tgKeyChatID: m.cfg.TargetChatID,
		"name":      name,
	}
	if emojiID != "" {
		params["icon_custom_emoji_id"] = emojiID
	}

	var out struct {
		MessageThreadID int64 `json:"message_thread_id"`
	}

	if err := m.tg(ctx, "createForumTopic", params, &out); err != nil {
		return 0, err
	}

	return out.MessageThreadID, nil
}

func (m *Manager) deleteForumTopic(ctx context.Context, topicID int64) error {
	return m.tg(ctx, "deleteForumTopic", map[string]any{
		tgKeyChatID:         m.cfg.TargetChatID,
		"message_thread_id": topicID,
	}, nil)
}

func (m *Manager) editForumTopic(ctx context.Context, topicID int64) error {
	err := m.tg(ctx, "editForumTopic", map[string]any{
		tgKeyChatID:         m.cfg.TargetChatID,
		"message_thread_id": topicID,
	}, nil)
	if err == nil {
		return nil
	}

	apiErr := &tgAPIError{}

	ok := errors.As(err, &apiErr)
	if !ok {
		return err
	}

	if strings.Contains(strings.ToLower(apiErr.Description), "not modified") {
		return nil
	}

	return err
}

func (m *Manager) sendMessageChunk(
	ctx context.Context,
	chunk string,
	markup *tgReplyMarkup,
	threadID int64,
) (int64, error) {
	params := map[string]any{
		tgKeyChatID:    m.cfg.TargetChatID,
		tgKeyText:      chunk,
		tgKeyParseMode: tgParseModeHTML,
	}
	if markup != nil {
		params["reply_markup"] = markup
	}

	if threadID > 0 {
		params["message_thread_id"] = threadID
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

func (m *Manager) sendPlainFallback(
	ctx context.Context,
	html string,
	markup *tgReplyMarkup,
	threadID int64,
) (int64, error) {
	params := map[string]any{
		tgKeyChatID: m.cfg.TargetChatID,
		tgKeyText:   stripHTMLToPlain(html),
	}
	if markup != nil {
		params["reply_markup"] = markup
	}

	if threadID > 0 {
		params["message_thread_id"] = threadID
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

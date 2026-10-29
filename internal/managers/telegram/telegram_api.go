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

type telegramBotUser struct {
	ID int64 `json:"id"`
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
		if isRetryableHTTPStatus(resp.StatusCode) {
			return &tgAPIError{
				Method:      method,
				Description: httpStatusDescription(resp),
				ErrorCode:   resp.StatusCode,
			}
		}

		return fmt.Errorf("parse telegram response: %w", err)
	}

	if isRetryableHTTPStatus(resp.StatusCode) {
		apiErr := &tgAPIError{
			Method:      method,
			Description: parsed.Description,
			ErrorCode:   resp.StatusCode,
		}
		if apiErr.Description == "" {
			apiErr.Description = httpStatusDescription(resp)
		}

		if parsed.Parameters != nil {
			apiErr.RetryAfter = parsed.Parameters.RetryAfter
		}

		return apiErr
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

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status < 600)
}

func httpStatusDescription(resp *http.Response) string {
	if resp.Status != "" {
		return resp.Status
	}

	return fmt.Sprintf("HTTP status %d", resp.StatusCode)
}

// sanitizeTransportError drops the *url.Error wrapper, whose text embeds the
// full request URL — for telegram that URL carries the bot token.
func sanitizeTransportError(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}

	return err
}

// disableLinkPreview suppresses URL preview cards on outgoing messages unless the caller set the option.
func disableLinkPreview(method string, params map[string]any) {
	if method != "sendMessage" {
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
		},
	}, &updates); err != nil {
		return nil, err
	}

	return updates, nil
}

func (m *Manager) getMe(ctx context.Context) (telegramBotUser, error) {
	var out telegramBotUser
	if err := m.tg(ctx, "getMe", nil, &out); err != nil {
		return telegramBotUser{}, err
	}

	return out, nil
}

func (m *Manager) getChat(ctx context.Context, chatID int64) (telegramChat, error) {
	var out telegramChat
	if err := m.tg(ctx, "getChat", map[string]any{tgKeyChatID: chatID}, &out); err != nil {
		return telegramChat{}, err
	}

	return out, nil
}

func (m *Manager) setCommands(ctx context.Context) error {
	return m.tg(ctx, "setMyCommands", map[string]any{"commands": publishedCommands}, nil)
}

func (m *Manager) sendTyping(ctx context.Context) error {
	return m.tg(ctx, "sendChatAction", map[string]any{
		tgKeyChatID: m.effectiveChatID(), "action": "typing",
	}, nil)
}

func (m *Manager) sendMessage(ctx context.Context, text string) (int64, error) {
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

		id, err := m.sendMessageChunk(ctx, chunk)
		if err != nil {
			if !shouldFallbackToPlain(err) {
				return 0, err
			}

			plainID, plainErr := m.sendPlainFallback(ctx, chunk)
			if plainErr != nil {
				return 0, plainErr
			}

			lastID = plainID

			continue
		}

		lastID = id
	}

	return lastID, nil
}

// Only an explicit parse rejection permits another send without duplicating an accepted chunk.
func shouldFallbackToPlain(err error) bool {
	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != http.StatusBadRequest {
		return false
	}

	return strings.Contains(strings.ToLower(apiErr.Description), "parse entities")
}

func (m *Manager) sendRawHTML(ctx context.Context, html string) (int64, error) {
	params := map[string]any{
		tgKeyChatID:    m.effectiveChatID(),
		tgKeyText:      html,
		tgKeyParseMode: tgParseModeHTML,
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

func (m *Manager) sendMessageChunk(ctx context.Context, chunk string) (int64, error) {
	params := map[string]any{
		tgKeyChatID:    m.effectiveChatID(),
		tgKeyText:      chunk,
		tgKeyParseMode: tgParseModeHTML,
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

func (m *Manager) sendPlainFallback(ctx context.Context, html string) (int64, error) {
	params := map[string]any{
		tgKeyChatID: m.effectiveChatID(),
		tgKeyText:   stripHTMLToPlain(html),
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}

	if err := m.tg(ctx, "sendMessage", params, &out); err != nil {
		return 0, err
	}

	return out.MessageID, nil
}

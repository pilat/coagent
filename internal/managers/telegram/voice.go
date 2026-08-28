package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

func (m *Manager) handleVoiceMessage(
	ctx context.Context,
	msg *telegramMessage,
	threadID, sessionID int64,
	hasSession bool,
) {
	if !hasSession {
		return
	}

	if msg.Voice == nil {
		return
	}

	_ = m.sendTyping(ctx, threadID)

	transcript, err := m.transcribeVoice(ctx, msg.Voice.FileID)
	if err != nil {
		_, _ = m.sendMessage(ctx, "Voice transcription failed: "+err.Error(), nil, threadID)
		return
	}

	_ = m.deleteMessage(ctx, msg.MessageID)
	_, _ = m.sendRawHTML(ctx, "🗣 <i>"+escapeHTML(transcript)+"</i>", nil, threadID)

	m.handleSessionMessage(ctx, sessionID, transcript, threadID)
}

func (m *Manager) transcribeVoice(ctx context.Context, fileID string) (string, error) {
	if m.cfg.Whisper == nil {
		return "", errors.New("voice transcription is not configured")
	}

	filePath, err := m.getTelegramFilePath(ctx, fileID, m.httpClient)
	if err != nil {
		return "", err
	}

	audio, err := m.downloadTelegramFile(ctx, filePath)
	if err != nil {
		return "", err
	}

	return m.transcribeAudio(ctx, audio)
}

func (m *Manager) getTelegramFilePath(ctx context.Context, fileID string, client *http.Client) (string, error) {
	var out struct {
		FilePath string `json:"file_path"`
	}

	if err := m.tgWithClient(ctx, client, "getFile", map[string]any{"file_id": fileID}, &out); err != nil {
		return "", err
	}

	if out.FilePath == "" {
		return "", errors.New("telegram returned empty file path")
	}

	return out.FilePath, nil
}

func (m *Manager) transcribeAudio(ctx context.Context, audio []byte) (string, error) {
	if m.cfg.Whisper == nil {
		return "", errors.New("voice transcription is not configured")
	}

	provider, ok := m.unifiedCfg.Providers[m.cfg.Whisper.Provider]
	if !ok {
		return "", fmt.Errorf("whisper provider %q not found", m.cfg.Whisper.Provider)
	}

	if provider.APIKey == "" {
		return "", fmt.Errorf("whisper provider %q has empty api_key", m.cfg.Whisper.Provider)
	}

	baseURL := provider.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	url := strings.TrimRight(baseURL, "/") + "/audio/transcriptions"

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	fileWriter, err := writer.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return "", fmt.Errorf("create voice form file: %w", err)
	}

	if _, err := fileWriter.Write(audio); err != nil {
		return "", fmt.Errorf("write voice audio: %w", err)
	}

	if err := writer.WriteField("model", m.cfg.Whisper.Model); err != nil {
		return "", fmt.Errorf("write whisper model field: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("build whisper request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call whisper API: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read whisper response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("whisper API error %d: %s", resp.StatusCode, string(raw))
	}

	var payload struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("parse whisper response: %w", err)
	}

	text := strings.TrimSpace(payload.Text)
	if text == "" {
		return "", errors.New("empty transcription")
	}

	return text, nil
}

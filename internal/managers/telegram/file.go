package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const attachmentDownloadTimeout = 10 * time.Minute

// Resolution and copying share saveAttachment's deadline because either phase
// can spend most of the transfer budget with a local Bot API server.
func (m *Manager) downloadToFile(ctx context.Context, filePath string, dst io.Writer) (int64, error) {
	src, err := m.openTelegramFile(ctx, filePath, m.downloadClient)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	written, err := io.Copy(dst, src)
	if err != nil {
		return 0, fmt.Errorf("write downloaded file: %w", err)
	}

	return written, nil
}

func (m *Manager) downloadTelegramFile(ctx context.Context, filePath string) ([]byte, error) {
	src, err := m.openTelegramFile(ctx, filePath, m.httpClient)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()

	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("read downloaded voice file: %w", err)
	}

	return raw, nil
}

func (m *Manager) openTelegramFile(ctx context.Context, filePath string, client *http.Client) (io.ReadCloser, error) {
	if filepath.IsAbs(filePath) {
		if m.cfg.APIURL == "" {
			return nil, errors.New("telegram returned a local file path without api_url")
		}

		file, err := os.Open(filePath)
		if err != nil {
			return nil, fmt.Errorf("open local telegram file: %w", err)
		}

		return file, nil
	}

	fileURL := fmt.Sprintf("%s/file/bot%s/%s", m.telegramAPIURL(), m.cfg.BotToken, filePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", sanitizeTransportError(err))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download telegram file: %w", sanitizeTransportError(err))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download telegram file failed with status %d", resp.StatusCode)
	}

	return resp.Body, nil
}

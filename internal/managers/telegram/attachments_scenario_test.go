package telegram

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Scenario-level contract (docs/testing.md): an upload driven through the real
// manager against a real daemon controller must leave exactly one durable
// session input whose text is the synthetic metadata message, byte-for-byte
// fixed in its static parts.
func TestHarnessScenario_AttachmentBecomesDurableSyntheticMessage(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	sessionID := h.produceBeforeManager(t)

	// Let the harness's own prompt turn finish before the upload arrives, so
	// the durable inbox holds at most that one consumed input.
	require.Eventually(t, func() bool {
		rec, err := h.sessions.GetSession(context.Background(), sessionID)
		return err == nil && rec.Status == sessionstore.SessionStatusCompleted
	}, 10*time.Second, 20*time.Millisecond)

	payload := []byte("%PDF-1.4 body")
	api := &fakeAttachmentAPI{getFilePath: "documents/report_1.pdf", payload: payload}
	manager, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", TargetChatID: targetID(harnessChatID),
	}, &config.UnifiedConfig{}, h.controller)
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripFunc(api.RoundTrip)}
	manager.httpClient = client
	manager.downloadClient = client

	msg := &telegramMessage{
		Caption: "quarterly numbers",
		Document: &telegramDocument{
			FileID: "doc-77", FileName: "report.pdf", MimeType: "application/pdf", FileSize: int64(len(payload)),
		},
	}

	manager.handleAttachment(context.Background(), msg, attachmentOf(msg), harnessTopicID, sessionID, true)

	// The pre-manager harness queued its own initial-prompt input first; skip
	// any pending inputs until the synthetic message surfaces.
	var stored string
	require.Eventually(t, func() bool {
		input, peekErr := h.sessions.PeekPending(context.Background(), sessionID)
		if peekErr != nil || !strings.HasPrefix(input.RawContent, syntheticHeader) {
			return false
		}

		stored = input.RawContent

		return true
	}, 10*time.Second, 20*time.Millisecond)

	wantPrefix := "The user attached a file:\n" +
		"- name: report.pdf\n" +
		fmt.Sprintf("- size: %s\n", payloadSizeText(payload)) +
		"- path: "
	require.True(t, strings.HasPrefix(stored, wantPrefix),
		"exact metadata template, got:\n%s", stored)

	savedPath, _, _ := strings.Cut(stored[len(wantPrefix):], "\n")
	assert.True(
		t,
		strings.HasPrefix(savedPath, filepath.Join(os.TempDir(), "coagent-")),
		"random temp artifact name, got %s",
		savedPath,
	)
	assert.True(t, strings.HasSuffix(savedPath, ".pdf"), "extension carried through sanitization")

	savedFile := extractedPath(stored)
	data, err := os.ReadFile(savedFile)
	require.NoError(t, err)
	assert.Equal(t, payload, data, "the saved file is byte-identical to the upload")

	assert.Contains(t, stored, "\nquarterly numbers")
	assert.NotContains(t, stored, imageAdvisory, "a pdf gets no image advice")
}

// The reported >20 MB update reaches the durable session-input boundary through
// local Bot API resolution and a filesystem copy.
func TestHarnessScenario_LargeLocalAttachmentBecomesDurableSyntheticMessage(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	sessionID := h.produceBeforeManager(t)

	require.Eventually(t, func() bool {
		rec, err := h.sessions.GetSession(context.Background(), sessionID)
		return err == nil && rec.Status == sessionstore.SessionStatusCompleted
	}, 10*time.Second, 20*time.Millisecond)

	source := filepath.Join(t.TempDir(), "menschen_a1_1_arbeitsbuch.pdf")
	payload := []byte("%PDF-1.4 workbook")
	require.NoError(t, os.WriteFile(source, payload, 0o600))

	api := &fakeAttachmentAPI{getFilePath: source}
	manager, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", APIURL: "http://127.0.0.1:8081",
		TargetChatID: targetID(harnessChatID),
	}, &config.UnifiedConfig{}, h.controller)
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripFunc(api.RoundTrip)}
	manager.httpClient = client
	manager.downloadClient = client

	msg := &telegramMessage{Document: &telegramDocument{
		FileID: "large-doc-77", FileName: filepath.Base(source), MimeType: "application/pdf",
		FileSize: 25 * 1024 * 1024,
	}}
	manager.handleAttachment(context.Background(), msg, attachmentOf(msg), harnessTopicID, sessionID, true)

	var stored string
	require.Eventually(t, func() bool {
		input, peekErr := h.sessions.PeekPending(context.Background(), sessionID)
		if peekErr != nil || !strings.HasPrefix(input.RawContent, syntheticHeader) {
			return false
		}
		stored = input.RawContent
		return true
	}, 10*time.Second, 20*time.Millisecond)

	savedPath := extractedPath(stored)
	t.Cleanup(func() { _ = os.Remove(savedPath) })
	saved, err := os.ReadFile(savedPath)
	require.NoError(t, err)
	assert.Equal(t, payload, saved)
	assert.Contains(t, stored, "- size: 25.0MB")
}

// A hosted-API size rejection must reach the final Telegram renderer with the
// supported local-mode remedy and must not create session input.
func TestHarnessScenario_HostedAPILargeAttachmentExplainsLimit(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	sessionID := h.produceBeforeManager(t)
	require.Eventually(t, func() bool {
		rec, err := h.sessions.GetSession(context.Background(), sessionID)
		return err == nil && rec.Status == sessionstore.SessionStatusCompleted
	}, 10*time.Second, 20*time.Millisecond)

	api := &fakeAttachmentAPI{getFileError: `{
		"ok":false,"error_code":400,"description":"Bad Request: file is too big"
	}`}
	manager, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", TargetChatID: targetID(harnessChatID),
	}, &config.UnifiedConfig{}, h.controller)
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripFunc(api.RoundTrip)}
	manager.httpClient = client
	manager.downloadClient = client

	msg := &telegramMessage{Document: &telegramDocument{
		FileID: "large-doc-77", FileName: "menschen_a1_1_arbeitsbuch.pdf", FileSize: 25 * 1024 * 1024,
	}}
	manager.handleAttachment(context.Background(), msg, attachmentOf(msg), harnessTopicID, sessionID, true)

	require.Len(t, api.sentMessages, 1)
	assert.Equal(t,
		"❌ Failed to save menschen_a1_1_arbeitsbuch.pdf: "+
			"telegram Bot API rejected this file as too big; files over 20 MB require "+
			"api_url pointing to a Bot API server running in local mode",
		api.sentMessages[0],
	)
	_, err = h.sessions.PeekPending(context.Background(), sessionID)
	require.ErrorContains(t, err, "no pending input")
}

func payloadSizeText(payload []byte) string {
	a := &tgAttachment{fileSize: int64(len(payload))}

	return a.sizeText()
}

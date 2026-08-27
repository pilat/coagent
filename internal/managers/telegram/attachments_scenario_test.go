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

func payloadSizeText(payload []byte) string {
	a := &tgAttachment{fileSize: int64(len(payload))}

	return a.sizeText()
}

package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// fakeAttachmentAPI serves getFile, the file download endpoint and records
// every sendMessage text, all through one transport.
type fakeAttachmentAPI struct {
	getFilePath  string
	payload      []byte
	downloadCode int // non-zero overrides 200 on the /file/ endpoint

	sentMessages []string
}

func (f *fakeAttachmentAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/getFile") {
		return telegramJSONResponse(`{"ok":true,"result":{"file_path":"` + f.getFilePath + `"}}`), nil
	}

	if strings.Contains(req.URL.Path, "/file/bot") {
		code := f.downloadCode
		if code == 0 {
			code = http.StatusOK
		}

		return &http.Response{
			StatusCode: code,
			Body:       io.NopCloser(strings.NewReader(string(f.payload))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	if !strings.Contains(req.URL.Path, "/sendMessage") {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		panic(err)
	}

	f.sentMessages = append(f.sentMessages, body.Text)

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7}}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func newAttachmentTestManager(t *testing.T, api *fakeAttachmentAPI, controller *fakeController) *Manager {
	t.Helper()

	client := &http.Client{Transport: roundTripFunc(api.RoundTrip)}

	return &Manager{
		id:             "telegram-main",
		cfg:            config.ManagerEntry{ID: "telegram-main", BotToken: "tok", TargetChatID: targetID(1)},
		controller:     controller,
		httpClient:     client,
		downloadClient: client,
	}
}

func pngPayload() []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}
}

func TestAttachment_DocumentSavedAndInjected(t *testing.T) {
	api := &fakeAttachmentAPI{getFilePath: "documents/report_1.pdf", payload: []byte("%PDF-1.4 body")}
	controller := &fakeController{}
	m := newAttachmentTestManager(t, api, controller)

	msg := &telegramMessage{
		Caption: "please review",
		Document: &telegramDocument{
			FileID: "doc-1", FileName: "report.pdf", MimeType: "application/pdf", FileSize: 16,
		},
	}

	m.handleAttachment(context.Background(), msg, attachmentOf(msg), 42, 99, true)

	require.Len(t, controller.messageCalls, 1)
	text := controller.messageCalls[0].Message
	assert.Contains(t, text, "- name: report.pdf")
	assert.Contains(t, text, "- size: 16B")
	assert.Contains(t, text, "- path: "+filepath.Join(os.TempDir(), "coagent-"))
	assert.True(t, strings.HasSuffix(text[:len("The user attached a file:")], "The user attached a file:"))
	assert.Contains(t, text, "\nplease review", "caption rides along verbatim")
	assert.NotContains(t, text, "read tool on this path", "a pdf earns no viewing advice")

	require.NotEmpty(t, api.sentMessages)
	assert.Contains(t, api.sentMessages[0], "📎 report.pdf")

	t.Cleanup(func() { _ = os.Remove(extractedPath(text)) })
}

func extractedPath(synthetic string) string {
	const marker = "- path: "
	for line := range strings.SplitSeq(synthetic, "\n") {
		if rest, ok := strings.CutPrefix(line, marker); ok {
			return rest
		}
	}

	return ""
}

func TestAttachment_NoCaptionOmitsSection(t *testing.T) {
	api := &fakeAttachmentAPI{getFilePath: "documents/doc.bin", payload: []byte("x")}
	controller := &fakeController{}
	m := newAttachmentTestManager(t, api, controller)

	msg := &telegramMessage{
		Document: &telegramDocument{FileID: "doc-2", FileName: "doc.bin"},
	}

	m.handleAttachment(context.Background(), msg, attachmentOf(msg), 42, 99, true)

	require.Len(t, controller.messageCalls, 1)
	text := controller.messageCalls[0].Message
	var nonEmpty int
	for line := range strings.SplitSeq(text, "\n") {
		if line != "" {
			nonEmpty++
		}
	}
	assert.Equal(t, 4, nonEmpty, "header + three metadata lines only")
	assert.NotContains(t, text, "\n\n", "no caption section when there is no caption")

	path := extractedPath(text)
	t.Cleanup(func() { _ = os.Remove(path) })
}

func TestAttachment_NoSessionDropsSilently(t *testing.T) {
	api := &fakeAttachmentAPI{getFilePath: "documents/x.pdf", payload: []byte("%PDF")}
	controller := &fakeController{}
	m := newAttachmentTestManager(t, api, controller)

	msg := &telegramMessage{Document: &telegramDocument{FileID: "doc-3"}}

	m.handleAttachment(context.Background(), msg, attachmentOf(msg), 42, 99, false)

	assert.Empty(t, controller.messageCalls)
	assert.Empty(t, api.sentMessages, "no-session topics produce no echo either")
}

func TestAttachment_DownloadFailureRepliesWithoutInjection(t *testing.T) {
	api := &fakeAttachmentAPI{getFilePath: "documents/x.pdf", downloadCode: http.StatusInternalServerError}
	controller := &fakeController{}
	m := newAttachmentTestManager(t, api, controller)

	msg := &telegramMessage{Document: &telegramDocument{FileID: "doc-4", FileName: "x.pdf"}}

	m.handleAttachment(context.Background(), msg, attachmentOf(msg), 42, 99, true)

	assert.Empty(t, controller.messageCalls, "a failed save injects nothing")
	require.Len(t, api.sentMessages, 1)
	assert.Contains(t, api.sentMessages[0], "Failed to save x.pdf")
}

func TestAttachment_LargestPhotoSelected(t *testing.T) {
	sizes := []telegramPhotoSize{
		{FileID: "small", Width: 90, Height: 90},
		{FileID: "big", Width: 1280, Height: 960},
		{FileID: "medium", Width: 320, Height: 320},
	}

	msg := &telegramMessage{Photo: sizes}
	att := attachmentOf(msg)
	require.NotNil(t, att)
	assert.Equal(t, "big", att.fileID)
}

func TestAttachment_ImageAdvisoryBoundaries(t *testing.T) {
	dir := t.TempDir()

	png := filepath.Join(dir, "at-cap.png")
	// PNG magic + filler == exactly the advisory cap
	prefix := []byte{0x89, 'P', 'N', 'G'}
	body := append(prefix, make([]byte, maxImageAdvisorySize-len(prefix))...)
	require.NoError(t, os.WriteFile(png, body, 0o600))

	withAdvice := buildSyntheticMessage("img.png", "5.0MB", png, "")
	assert.Contains(t, withAdvice, imageAdvisory, "an at-cap image is readable and gets advice")

	big := filepath.Join(dir, "over.png")
	bigBody := append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, maxImageAdvisorySize)...)
	require.NoError(t, os.WriteFile(big, bigBody, 0o600))

	pngOverCap := buildSyntheticMessage("over.png", "5.1MB", big, "")
	assert.NotContains(t, pngOverCap, imageAdvisory, "above cap there must be no advice")

	pdf := filepath.Join(dir, "doc.pdf")
	require.NoError(t, os.WriteFile(pdf, []byte("%PDF-1.4 ..."), 0o600))

	noPixels := buildSyntheticMessage("doc.pdf", "12B", pdf, "")
	assert.NotContains(t, noPixels, imageAdvisory, "sniffed-PDF gets metadata only")
}

func TestAttachment_SavedImageCarriesReadAdviceAndRealType(t *testing.T) {
	api := &fakeAttachmentAPI{getFilePath: "photos/file_0.jpg", payload: pngPayload()}
	controller := &fakeController{}
	m := newAttachmentTestManager(t, api, controller)

	msg := &telegramMessage{Photo: []telegramPhotoSize{{FileID: "photo-1", Width: 640, Height: 480}}}

	m.handleAttachment(context.Background(), msg, attachmentOf(msg), 42, 99, true)

	require.Len(t, controller.messageCalls, 1)
	text := controller.messageCalls[0].Message

	assert.Contains(t, text, "- name: photo.jpg")
	assert.Contains(t, text, imageAdvisory, "small sniffed image gets exactly one advisory sentence")
	assert.Equal(t, 1, strings.Count(text, "Use the read tool"), "never double advice")
}

func TestAttachment_PhotoAndAudioNameFallbacks(t *testing.T) {
	audio := attachmentOf(&telegramMessage{
		Audio: &telegramAudio{FileID: "a1", MimeType: "audio/mpeg", FileName: ""},
	})
	assert.Equal(t, "audio.mpeg", audio.nameText("/tmp/coagent-whatever"))

	doc := attachmentOf(&telegramMessage{
		Document: &telegramDocument{FileID: "d1", FileName: "", MimeType: ""},
	})
	assert.Equal(t, "coagent-whatever", doc.nameText("/tmp/coagent-whatever"))
}

func TestAttachment_UnnamedDocumentDoesNotInheritAudioPrefix(t *testing.T) {
	doc := attachmentOf(&telegramMessage{
		Document: &telegramDocument{FileID: "d1", FileName: "", MimeType: "application/pdf"},
	})

	assert.Equal(t, "coagent-whatever", doc.nameText("/tmp/coagent-whatever"),
		"mime-derived fallback is audio-only per D12; unnamed documents keep the saved-path base")
}

func TestSanitizeExtension_StripsNonAlnumAndCapsLength(t *testing.T) {
	tests := map[string]string{
		"":                                  "",
		".pdf":                              ".pdf",
		"pdf":                               ".pdf",
		"/../evil!!":                        ".evil",
		strings.Repeat(".longextension", 2): ".longextens",
		"日本語":                               "",
	}

	for raw, want := range tests {
		assert.Equal(t, want, sanitizeExtension(raw), "raw=%q", raw)
	}
}

func TestFirstExtension_PrefersLongestCandidate(t *testing.T) {
	assert.Equal(t, "jpeg", firstExtension("", "photos/file_0.jpeg"))
	assert.Equal(t, "pdf", firstExtension("report.pdf", ""))
	assert.Empty(t, firstExtension("", ""))
}

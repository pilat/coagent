package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestVoice_TranscribesEchoesAndForwardsToSelectedSession(t *testing.T) {
	var messages []string
	var methods []string
	controller := &fakeController{focus: 42}
	manager := newCmdTestManager(t, controller, &messages)
	manager.cfg.Whisper = &config.ManagerWhisperEntry{Provider: "speech", Model: "test-transcription"}
	manager.unifiedCfg = &config.UnifiedConfig{Providers: map[string]config.ProviderEntry{
		"speech": {APIKey: "test-key", BaseURL: "https://speech.invalid"},
	}}
	manager.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := filepath.Base(req.URL.Path)
		if strings.Contains(req.URL.Path, "/file/bot") {
			method = "download"
		}
		methods = append(methods, method)
		switch method {
		case "getFile":
			return telegramResponse(req, `{"ok":true,"result":{"file_path":"voice/file.ogg"}}`), nil
		case "download":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("audio"))}, nil
		case "transcriptions":
			return telegramResponse(req, `{"text":"hello <voice>"}`), nil
		case "sendMessage":
			var payload struct {
				Text string `json:"text"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
			messages = append(messages, payload.Text)
			return telegramResponse(req, `{"ok":true,"result":{"message_id":1}}`), nil
		default:
			return telegramResponse(req, `{"ok":true,"result":true}`), nil
		}
	})}
	manager.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		MessageID: 10, From: &telegramUser{ID: 42}, Chat: telegramChat{ID: harnessChatID},
		Voice: &telegramVoiceMessage{FileID: "voice-file"},
	}})
	assert.Equal(t, []string{"sendChatAction", "getFile", "download", "transcriptions", "sendMessage"}, methods)
	assert.Equal(t, []string{"🗣 <i>hello &lt;voice&gt;</i>"}, messages)
	require.Len(t, controller.messageCalls, 1)
	assert.Equal(t, int64(42), controller.messageCalls[0].SessionID)
	assert.Equal(t, "hello <voice>", controller.messageCalls[0].Message)
}

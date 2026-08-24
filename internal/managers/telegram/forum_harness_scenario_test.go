package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestHarnessScenario_BotForumStartupGeneralAndRestart(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()
	require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

	remote := &botForumHarness{}
	first := remote.newManager(t)
	require.NoError(t, first.Start(context.Background()))
	first.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: 7}, Text: "hello",
	}})
	require.NoError(t, first.Stop(context.Background()))

	assert.Equal(t, []string{
		"getMe", "getChat", "createForumTopic", "setMyCommands", "sendMessage",
	}, remote.prefix())
	assert.Equal(t, 1, remote.created)
	assert.Equal(t, int64(0), remote.sentThreads[0])
	assert.Equal(t, "Open the “Private support” topic to create or manage sessions.", remote.sentTexts[0])

	remote.resetCalls()
	second := remote.newManager(t)
	require.NoError(t, second.Start(context.Background()))
	require.NoError(t, second.Stop(context.Background()))

	assert.Equal(t, []string{"getMe", "getChat", "editForumTopic", "setMyCommands"}, remote.prefix())
	assert.Equal(t, 1, remote.created, "restart must not create another service topic")
	assert.Equal(t, []forumTopicUpdate{{
		ChatID: 7, ThreadID: 7001,
	}}, remote.topicUpdates)
}

type botForumHarness struct {
	mu           sync.Mutex
	methods      []string
	created      int
	sentTexts    []string
	sentThreads  []int64
	topicUpdates []forumTopicUpdate
}

type forumTopicUpdate struct {
	ChatID      int64  `json:"chat_id"`
	ThreadID    int64  `json:"message_thread_id"`
	Name        string `json:"name"`
	IconEmojiID string `json:"icon_custom_emoji_id"`
	Text        string `json:"text"`
}

func (h *botForumHarness) newManager(t *testing.T) *Manager {
	t.Helper()
	entry := config.ManagerEntry{
		ID: "telegram-main", BotToken: "test", AllowedUserIDs: []int64{7},
		ServiceTopicName: "Private support", ServiceTopicIconEmojiID: "888",
		SendChunkDelayMS: 0, PollTimeoutSec: 1,
	}
	m, err := New(entry, nil, &fakeController{})
	require.NoError(t, err)
	m.httpClient = &http.Client{Transport: roundTripFunc(h.roundTrip)}
	return m
}

func (h *botForumHarness) roundTrip(req *http.Request) (*http.Response, error) {
	method := filepath.Base(req.URL.Path)
	var body forumTopicUpdate
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&body)
	}
	h.mu.Lock()
	h.methods = append(h.methods, method)
	if method == "sendMessage" {
		h.sentTexts = append(h.sentTexts, body.Text)
		h.sentThreads = append(h.sentThreads, body.ThreadID)
	}
	if method == "createForumTopic" {
		h.created++
	}
	if method == "editForumTopic" {
		h.topicUpdates = append(h.topicUpdates, body)
	}
	h.mu.Unlock()

	switch method {
	case "getMe":
		return harnessResponse(req, `{"ok":true,"result":{"id":9,"has_topics_enabled":true}}`), nil
	case "getChat":
		return harnessResponse(req, `{"ok":true,"result":{"id":7,"type":"private"}}`), nil
	case "createForumTopic":
		return harnessResponse(req, `{"ok":true,"result":{"message_thread_id":7001}}`), nil
	case "editForumTopic":
		return harnessResponse(
			req,
			`{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`,
		), nil
	case "getUpdates":
		<-req.Context().Done()
		return nil, req.Context().Err()
	default:
		return harnessResponse(req, `{"ok":true,"result":{"message_id":1}}`), nil
	}
}

func (h *botForumHarness) prefix() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	methods := make([]string, 0, len(h.methods))
	for _, method := range h.methods {
		if method != "getUpdates" {
			methods = append(methods, method)
		}
	}
	return methods
}

func (h *botForumHarness) resetCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.methods = nil
}

func harnessResponse(req *http.Request, payload string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    req,
	}
}

package telegram

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestHTTPTimeoutForPollTimeout(t *testing.T) {
	tests := []struct {
		poll int
		want time.Duration
	}{
		{poll: 0, want: 45 * time.Second},
		{poll: 30, want: 45 * time.Second},
		{poll: 45, want: 60 * time.Second},
	}
	for _, tt := range tests {
		got, err := httpTimeoutFor(tt.poll)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got)
	}

	_, err := httpTimeoutFor(int(^uint(0) >> 1))
	assert.Error(t, err)
}

func TestTextPreview(t *testing.T) {
	assert.Empty(t, textPreview(""))
	assert.Equal(t, "short message", textPreview("short message"))

	// 60 ASCII runes → truncated to 48 + ellipsis (49 runes).
	long := textPreview(strings.Repeat("a", 60))
	assert.Len(t, []rune(long), 49)
	assert.True(t, strings.HasSuffix(long, "…"))

	// Multibyte must truncate on a rune boundary, never split a rune.
	cyr := textPreview(strings.Repeat("я", 60))
	assert.Len(t, []rune(cyr), 49)
	assert.True(t, strings.HasSuffix(cyr, "…"))
	assert.True(t, strings.HasPrefix(cyr, "я"))
}

func TestStartDoesNotAnnounceStartup(t *testing.T) {
	home := t.TempDir()
	restoreHome := coagenthome.Override(home)
	defer restoreHome()

	require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

	methods := make([]string, 0, 2)
	manager := &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID:             "telegram-main",
			BotToken:       "token",
			TargetChatID:   targetID(-100123),
			PollTimeoutSec: 30,
		},
		target:     forumTarget{chatID: -100123, topology: forumTopologyGroup},
		botUserID:  42,
		controller: &fakeController{},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			methods = append(methods, method)
			if method == "getUpdates" {
				<-req.Context().Done()

				return nil, req.Context().Err()
			}

			switch method {
			case "getMe":
				return telegramResponse(req, `{"ok":true,"result":{"id":42}}`), nil
			case "getChat":
				return telegramResponse(
					req,
					`{"ok":true,"result":{"id":-100123,"type":"supergroup","is_forum":true}}`,
				), nil
			case "getChatMember":
				return telegramResponse(
					req,
					`{"ok":true,"result":{"status":"administrator","can_manage_topics":true,"can_delete_messages":true}}`,
				), nil
			default:
				return telegramResponse(req, `{"ok":true,"result":true}`), nil
			}
		})},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}
	require.NoError(t, manager.saveServiceTopicID(7001))

	require.NoError(t, manager.Start(context.Background()))
	require.NoError(t, manager.Stop(context.Background()))

	assert.NotContains(t, methods, "sendMessage")
}

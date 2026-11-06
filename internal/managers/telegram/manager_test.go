package telegram

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	recorder := &delayedTelegramRecorder{}
	manager, err := New(config.ManagerEntry{
		ID: "telegram-main", BotToken: "test-token", AllowedUserIDs: []int64{harnessChatID},
	}, nil, &fakeController{})
	require.NoError(t, err)
	manager.httpClient = &http.Client{Transport: recorder}
	require.NoError(t, manager.Start(t.Context()))
	require.NoError(t, manager.Stop(t.Context()))
	var methods []string
	for _, call := range recorder.snapshot() {
		methods = append(methods, call.Method)
	}
	assert.Equal(t, []string{"getMe", "getChat", "setMyCommands"}, methods)
	assert.False(t, manager.Alive())
}

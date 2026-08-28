package configtools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
)

func TestParseManagerParams_MapsEachPropertyIndependently(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		check func(*testing.T, configops.ManagerPatch)
	}{
		{"driver", `"driver":"telegram"`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.Driver)
			assert.Equal(t, "telegram", *p.Driver)
		}},
		{"enabled", `"enabled":false`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.Enabled)
			assert.False(t, *p.Enabled)
		}},
		{"bot_token", `"bot_token":"${TG_TOKEN}"`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.BotToken)
			assert.Equal(t, "${TG_TOKEN}", *p.BotToken)
		}},
		{"api_url", `"api_url":"http://127.0.0.1:8081"`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.APIURL)
			assert.Equal(t, "http://127.0.0.1:8081", *p.APIURL)
		}},
		{"allowed_user_ids", `"allowed_user_ids":[7]`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.AllowedUserIDs)
			assert.Equal(t, []int64{7}, *p.AllowedUserIDs)
		}},
		{"target_chat_id", `"target_chat_id":-100`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.TargetChatID)
			assert.Equal(t, int64(-100), *p.TargetChatID)
		}},
		{"service_topic_name", `"service_topic_name":"Support"`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.ServiceTopicName)
			assert.Equal(t, "Support", *p.ServiceTopicName)
		}},
		{
			"service_topic_icon_emoji_id",
			`"service_topic_icon_emoji_id":"123"`,
			func(t *testing.T, p configops.ManagerPatch) {
				require.NotNil(t, p.ServiceTopicIconEmojiID)
				assert.Equal(t, "123", *p.ServiceTopicIconEmojiID)
			},
		},
		{
			"session_topic_icon_emoji_id",
			`"session_topic_icon_emoji_id":"456"`,
			func(t *testing.T, p configops.ManagerPatch) {
				require.NotNil(t, p.SessionTopicIconEmojiID)
				assert.Equal(t, "456", *p.SessionTopicIconEmojiID)
			},
		},
		{"send_chunk_delay_ms", `"send_chunk_delay_ms":12`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.SendChunkDelayMS)
			assert.Equal(t, 12, *p.SendChunkDelayMS)
		}},
		{"poll_timeout_sec", `"poll_timeout_sec":30`, func(t *testing.T, p configops.ManagerPatch) {
			require.NotNil(t, p.PollTimeoutSec)
			assert.Equal(t, 30, *p.PollTimeoutSec)
		}},
		{
			"whisper",
			`"whisper":{"provider":"openai","model":"whisper-1"}`,
			func(t *testing.T, p configops.ManagerPatch) {
				require.True(t, p.Whisper.Set)
				require.NotNil(t, p.Whisper.Value)
				assert.Equal(t, "openai", p.Whisper.Value.Provider)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			patch, err := parseManagerParams([]byte(`{"id":"tg",` + tt.json + `}`))
			require.NoError(t, err)
			tt.check(t, patch)
			assert.Equal(t, "tg", patch.ID)
			assertOptionalManagerFieldAbsence(t, tt.name, patch)
		})
	}
}

func assertOptionalManagerFieldAbsence(t *testing.T, present string, p configops.ManagerPatch) {
	t.Helper()
	if present != "driver" {
		assert.Nil(t, p.Driver)
	}
	if present != "enabled" {
		assert.Nil(t, p.Enabled)
	}
	if present != "bot_token" {
		assert.Nil(t, p.BotToken)
	}
	if present != "api_url" {
		assert.Nil(t, p.APIURL)
	}
	if present != "allowed_user_ids" {
		assert.Nil(t, p.AllowedUserIDs)
	}
	if present != "target_chat_id" {
		assert.Nil(t, p.TargetChatID)
	}
	if present != "service_topic_name" {
		assert.Nil(t, p.ServiceTopicName)
	}
	if present != "service_topic_icon_emoji_id" {
		assert.Nil(t, p.ServiceTopicIconEmojiID)
	}
	if present != "session_topic_icon_emoji_id" {
		assert.Nil(t, p.SessionTopicIconEmojiID)
	}
	if present != "send_chunk_delay_ms" {
		assert.Nil(t, p.SendChunkDelayMS)
	}
	if present != "poll_timeout_sec" {
		assert.Nil(t, p.PollTimeoutSec)
	}
	if present != "whisper" {
		assert.False(t, p.Whisper.Set)
	}
}

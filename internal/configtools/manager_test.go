package configtools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestParseManagerParams_AcceptsAllFields(t *testing.T) {
	t.Parallel()

	params := `{
		"id": "tg-main",
		"driver": "telegram",
		"enabled": true,
		"bot_token": "${TG_BOT_TOKEN}",
		"api_url": "http://127.0.0.1:8081",
		"allowed_user_ids": [7, 9],
		"target_chat_id": -100,
		"service_topic_name": "Support",
		"service_topic_icon_emoji_id": "123",
		"session_topic_icon_emoji_id": "456",
		"send_chunk_delay_ms": 200,
		"poll_timeout_sec": 45,
		"whisper": {"provider": "openai", "model": "whisper-1"}
	}`

	patch, err := parseManagerParams([]byte(params))
	require.NoError(t, err)

	assert.Equal(t, "tg-main", patch.ID)
	require.NotNil(t, patch.Driver)
	assert.Equal(t, "telegram", *patch.Driver)
	require.NotNil(t, patch.Enabled)
	assert.True(t, *patch.Enabled)
	require.NotNil(t, patch.BotToken)
	assert.Equal(t, "${TG_BOT_TOKEN}", *patch.BotToken)
	require.NotNil(t, patch.APIURL)
	assert.Equal(t, "http://127.0.0.1:8081", *patch.APIURL)
	require.NotNil(t, patch.AllowedUserIDs)
	assert.Equal(t, []int64{7, 9}, *patch.AllowedUserIDs)
	require.NotNil(t, patch.TargetChatID)
	assert.Equal(t, int64(-100), *patch.TargetChatID)
	require.NotNil(t, patch.ServiceTopicName)
	assert.Equal(t, "Support", *patch.ServiceTopicName)
	require.NotNil(t, patch.ServiceTopicIconEmojiID)
	assert.Equal(t, "123", *patch.ServiceTopicIconEmojiID)
	require.NotNil(t, patch.SessionTopicIconEmojiID)
	assert.Equal(t, "456", *patch.SessionTopicIconEmojiID)
	require.NotNil(t, patch.SendChunkDelayMS)
	assert.Equal(t, 200, *patch.SendChunkDelayMS)
	require.NotNil(t, patch.PollTimeoutSec)
	assert.Equal(t, 45, *patch.PollTimeoutSec)
	assert.True(t, patch.Whisper.Set)
	require.NotNil(t, patch.Whisper.Value)
	assert.Equal(t, "openai", patch.Whisper.Value.Provider)
	assert.Equal(t, "whisper-1", patch.Whisper.Value.Model)
}

func TestParseManagerParams_RequiredFieldsOnly(t *testing.T) {
	t.Parallel()

	patch, err := parseManagerParams([]byte(`{"id":"tg"}`))
	require.NoError(t, err)
	assert.Equal(t, "tg", patch.ID)
	assert.Nil(t, patch.Driver)
	assert.Nil(t, patch.Enabled)
	assert.Nil(t, patch.BotToken)
	assert.Nil(t, patch.APIURL)
	assert.False(t, patch.Whisper.Set)
}

func TestParseManagerParams_RejectsMissingID(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"driver":"telegram"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
}

func TestParseManagerParams_RejectsUnknownField(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg","unknown":"value"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestParseManagerParams_RejectsDuplicateKey(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg","id":"tg2"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate key")
}

func TestParseManagerParams_RejectsTrailingData(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg"} "extra"`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data")
}

func TestParseManagerParams_RejectsNullForNonNullableFields(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{"driver", `{"id":"tg","driver":null}`, "driver cannot be null"},
		{"enabled", `{"id":"tg","enabled":null}`, "enabled cannot be null"},
		{"bot_token", `{"id":"tg","bot_token":null}`, "bot_token cannot be null"},
		{"api_url", `{"id":"tg","api_url":null}`, "api_url cannot be null"},
		{"allowed_user_ids", `{"id":"tg","allowed_user_ids":null}`, "allowed_user_ids cannot be null"},
		{"target_chat_id", `{"id":"tg","target_chat_id":null}`, "target_chat_id cannot be null"},
		{"service_topic_name", `{"id":"tg","service_topic_name":null}`, "service_topic_name cannot be null"},
		{
			"service_topic_icon_emoji_id",
			`{"id":"tg","service_topic_icon_emoji_id":null}`,
			"service_topic_icon_emoji_id cannot be null",
		},
		{
			"session_topic_icon_emoji_id",
			`{"id":"tg","session_topic_icon_emoji_id":null}`,
			"session_topic_icon_emoji_id cannot be null",
		},
		{"send_chunk_delay_ms", `{"id":"tg","send_chunk_delay_ms":null}`, "send_chunk_delay_ms cannot be null"},
		{"poll_timeout_sec", `{"id":"tg","poll_timeout_sec":null}`, "poll_timeout_sec cannot be null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseManagerParams([]byte(tt.params))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestParseManagerParams_AcceptsWhisperNull(t *testing.T) {
	t.Parallel()

	patch, err := parseManagerParams([]byte(`{"id":"tg","whisper":null}`))
	require.NoError(t, err)
	assert.True(t, patch.Whisper.Set)
	assert.Nil(t, patch.Whisper.Value)
}

func TestParseManagerParams_RejectsNullInAllowedUserIDs(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg","allowed_user_ids":[7,null]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null")
}

func TestParseManagerParams_RejectsUnknownWhisperField(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg","whisper":{"provider":"x","model":"y","extra":"z"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whisper")
}

func TestParseManagerParams_RejectsMissingWhisperFields(t *testing.T) {
	t.Parallel()

	_, err := parseManagerParams([]byte(`{"id":"tg","whisper":{"provider":"x"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whisper provider and model are required")
}

func TestParseManagerParams_RejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{"id":"tg","Driver":"telegram"}`,
		`{"id":"tg","whisper":{"provider":"x","model":"y","Provider":"z"}}`,
		`{"id":"tg","whisper":{"provider":"x","model":"y","provider":"z"}}`,
		`{"id":"tg","whisper":{"provider":null,"model":"y"}}`,
		`{"id":"tg"} trailing`,
	}
	for _, input := range tests {
		_, err := parseManagerParams([]byte(input))
		assert.Error(t, err, input)
	}
}

func TestSetManagerSchemaCoversManagerFields(t *testing.T) {
	t.Parallel()

	var schema map[string]json.RawMessage
	require.NoError(t, json.Unmarshal((&setManagerTool{}).Parameters(), &schema))
	var additional bool
	require.NoError(t, json.Unmarshal(schema["additionalProperties"], &additional))
	assert.False(t, additional)
	var properties map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(schema["properties"], &properties))

	var whisper map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(properties["whisper"], &whisper))
	require.NoError(t, json.Unmarshal(whisper["additionalProperties"], &additional))
	assert.False(t, additional)
	var whisperProperties map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(whisper["properties"], &whisperProperties))

	typ := reflect.TypeFor[config.ManagerEntry]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			assert.Contains(t, properties, tag)
		}
	}
	whisperType := reflect.TypeFor[config.ManagerWhisperEntry]()
	for i := range whisperType.NumField() {
		tag := strings.Split(whisperType.Field(i).Tag.Get("yaml"), ",")[0]
		assert.Contains(t, whisperProperties, tag)
	}
}

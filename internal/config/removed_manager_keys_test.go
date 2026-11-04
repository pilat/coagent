package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedConfig_RemovedManagerKeysAreStrippedAndRecordedOnce(t *testing.T) {
	cfg, err := ParseAndResolve([]byte(`providers:
  test: {driver: anthropic, api_key: test-key}
managers:
  - &manager
    id: first
    driver: telegram
    enabled: true
    bot_token: test-token
    allowed_user_ids: [7]
    service_topic_name: old
    service_topic_icon_emoji_id: old
    session_topic_icon_emoji_id: old
  - <<: *manager
    id: second
    service_topic_name: ignored
`), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"service_topic_icon_emoji_id", "service_topic_name", "session_topic_icon_emoji_id",
	}, cfg.RemovedManagerKeys)
	require.Len(t, cfg.Managers, 2)
	encoded, err := MarshalUnifiedConfig(cfg)
	require.NoError(t, err)
	for key := range removedManagerKeys {
		assert.NotContains(t, string(encoded), key)
	}

	entry := reflect.TypeFor[ManagerEntry]()
	for field := range entry.Fields() {
		for key := range removedManagerKeys {
			assert.NotContains(t, field.Tag.Get("yaml"), key)
		}
	}
}

func TestUnifiedConfig_RemovedKeysDoNotHideUnknownFieldsOrTrailingDocuments(t *testing.T) {
	for _, input := range []string{
		"managers:\n  - service_topic_name: old\n    poll_timeot_sec: 1\n",
		"managers:\n  - service_topic_name: old\n---\nmodels: []\n",
		"service_topic_name: unsupported outside managers\n",
	} {
		_, err := ParseUnifiedConfig([]byte(input))
		require.Error(t, err)
	}
}

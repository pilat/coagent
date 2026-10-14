package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedConfig_AssistantTopicName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		bad  bool
	}{
		{want: "Assistant"},
		{name: "Desk", want: "Desk"},
		{name: strings.Repeat("я", 128), want: strings.Repeat("я", 128)},
		{name: strings.Repeat("я", 129), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, `providers:
  test:
    driver: anthropic
    api_key: key
managers:
  - id: tg
    driver: telegram
    enabled: true
    bot_token: token
    allowed_user_ids: [7]
    assistant_topic_name: `+tc.name+"\n")
			cfg, err := LoadUnifiedConfig(path, nil)
			if tc.bad {
				require.ErrorContains(t, err, "assistant_topic_name")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Managers[0].AssistantTopicName)
		})
	}
}

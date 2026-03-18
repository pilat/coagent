package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestNewOpenAIClientClampsMaxTokens(t *testing.T) {
	tests := []struct {
		name          string
		maxTokens     int
		contextWindow int
		want          int
	}{
		{
			name:          "degenerate catalog reports window as output limit",
			maxTokens:     262144,
			contextWindow: 262144,
			want:          39321,
		},
		{
			name:          "honest but large output limit",
			maxTokens:     100352,
			contextWindow: 262144,
			want:          39321,
		},
		{
			name:          "small output limit stays untouched",
			maxTokens:     8192,
			contextWindow: 262144,
			want:          8192,
		},
		{
			name:          "absent output limit stays omitted",
			maxTokens:     0,
			contextWindow: 262144,
			want:          0,
		},
		{
			name:          "no window means nothing to clamp against",
			maxTokens:     100352,
			contextWindow: 0,
			want:          100352,
		},
		{
			name:          "window too small to carry a reserve never zeroes the limit",
			maxTokens:     4,
			contextWindow: 6,
			want:          4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newOpenAIClient("https://example.test", "key", "openrouter", nil, config.ModelEntry{
				ID:            "moonshotai/kimi-k2.5",
				MaxTokens:     tt.maxTokens,
				ContextWindow: tt.contextWindow,
			})

			assert.Equal(t, tt.want, client.maxTokens)
		})
	}
}

func TestChatRequestCarriesClampedMaxTokens(t *testing.T) {
	body := captureChatRequest(t, openAICompatibleParams{
		APIKey: "key",
		Model: config.ModelEntry{
			ID:            "moonshotai/kimi-k2.5",
			MaxTokens:     262144,
			ContextWindow: 262144,
		},
	})

	maxTokens, ok := body["max_tokens"].(float64)
	require.True(t, ok)
	assert.Equal(t, 39321, int(maxTokens))
}

func TestChatRequestOmitsAbsentMaxTokens(t *testing.T) {
	body := captureChatRequest(t, openAICompatibleParams{
		APIKey: "key",
		Model: config.ModelEntry{
			ID:            "moonshotai/kimi-k2.5",
			ContextWindow: 262144,
		},
	})

	assert.NotContains(t, body, "max_tokens")
}

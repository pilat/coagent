//go:build live

package llm

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// TestOpenRouterNativeSearchLive exercises the OpenRouter server-tool web
// search against the real API. Opt-in and billed to the provided key.
//
// Run: OPENROUTER_API_KEY=... OPENROUTER_MODEL=openai/gpt-5.2 \
//
//	go test -tags=live -v -run TestOpenRouterNativeSearchLive ./internal/llm
func TestOpenRouterNativeSearchLive(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		t.Skip("OPENROUTER_MODEL not set")
	}

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       apiKey,
		Model:        config.ModelEntry{ID: model},
		IsOpenRouter: true,
		NativeSearch: true,
	})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), "Answer briefly and cite sources.",
		[]llmwire.Message{{Role: llmwire.RoleUser, Content: "What is the current stable Go release?"}},
		nativeSearchTools())
	require.NoError(t, err)

	assert.NotEmpty(t, resp.Text, "the grounded answer carries inline citations")
	assert.Equal(t, "stop", resp.FinishType)
}

//go:build live

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// TestCacheMultiTurn reproduces the exact flow of the agent loop:
// - Large system prompt (CLAUDE.md)
// - First user message (CLAUDE.md preferences)
// - Tools registered
// - Multi-turn conversation
//
// Verifies caching works on the second turn (cache_read > 0).
//
// Run: OPENAI_API_KEY=... OPENAI_BASE_URL=https://openrouter.ai/api/v1 OPENAI_MODEL=... \
// go test -tags=live -v -run TestCacheMultiTurn ./internal/llm
func TestCacheMultiTurn(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" || !strings.Contains(baseURL, "openrouter") {
		t.Skip("OPENAI_BASE_URL not set or not OpenRouter")
	}

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		t.Skip("OPENAI_MODEL not set")
	}

	testCacheMultiTurnWithModel(t, baseURL, apiKey, model)
}

func testCacheMultiTurnWithModel(t *testing.T, baseURL, apiKey, model string) {
	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   config.ModelEntry{ID: model},
	})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	// Use the real CLAUDE.md-sized system prompt (~5000 tokens).
	var sb strings.Builder
	sb.WriteString(
		"You are an autonomous AI agent. You have access to tools for file operations, search, and shell commands.\n\n",
	)
	sb.WriteString("# Instructions\n\n")
	for i := range 200 {
		fmt.Fprintf(&sb, "## Rule %d\n", i+1)
		sb.WriteString("When performing code modifications, always read the file first before editing. ")
		sb.WriteString("Use structured output and follow the project coding style. ")
		sb.WriteString("Prefer minimal changes that solve the problem directly.\n\n")
	}
	systemPrompt := sb.String()

	// Register tools like the real loop does
	tools := []llmwire.ToolSchema{
		{
			Name:        "read",
			Description: "Read a file from disk",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"file_path":{"type":"string","description":"Path to read"}},"required":["file_path"]}`,
			),
		},
		{
			Name:        "write",
			Description: "Write content to a file",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`,
			),
		},
		{
			Name:        "bash",
			Description: "Execute a shell command",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`,
			),
		},
		{
			Name:        "grep",
			Description: "Search file contents",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`,
			),
		},
		{
			Name:        "glob",
			Description: "Find files by pattern",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`,
			),
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// --- Turn 1: First user message (like CLAUDE.md preferences loaded by session) ---
	messages1 := make([]llmwire.Message, 0, 3)
	messages1 = append(messages1, llmwire.Message{
		Role:    "user",
		Content: "User preferences from CLAUDE.md files:\n\n- We prefer Go code style with short variable names\n- Use table-driven tests\n- Error wrapping with fmt.Errorf\n\nThe user says: Say 'hello' and nothing else.",
	})

	resp1, err := client.Chat(ctx, systemPrompt, messages1, tools)
	require.NoError(t, err)
	require.NotNil(t, resp1)
	require.NotNil(t, resp1.Usage)

	usage1 := resp1.Usage

	t.Logf("Turn 1: prompt=%d, completion=%d, cache_read=%d, cache_write=%d, response=%q",
		usage1.PromptTokens, usage1.CompletionTokens, usage1.CacheTokens, usage1.CacheWriteTokens,
		truncate(resp1.Text, 100))

	// First turn should write to cache
	if usage1.CacheWriteTokens == 0 {
		t.Log("WARNING: first turn did not write to cache — caching may not be enabled for this provider")
	}

	// --- Turn 2: Add assistant response + new user message (simulates loop iteration 2) ---
	messages2 := append(messages1,
		llmwire.Message{
			Role:    "assistant",
			Content: resp1.Text,
		},
		llmwire.Message{
			Role:    "user",
			Content: "Now say 'world' and nothing else.",
		},
	)

	resp2, err := client.Chat(ctx, systemPrompt, messages2, tools)
	require.NoError(t, err)
	require.NotNil(t, resp2)
	require.NotNil(t, resp2.Usage)

	usage2 := resp2.Usage

	t.Logf("Turn 2: prompt=%d, completion=%d, cache_read=%d, cache_write=%d, response=%q",
		usage2.PromptTokens, usage2.CompletionTokens, usage2.CacheTokens, usage2.CacheWriteTokens,
		truncate(resp2.Text, 100))

	// Second turn MUST read from cache (system prompt + first user message are identical)
	assert.Positive(t, usage2.CacheTokens,
		"second turn should read from cache (cached_tokens > 0)")

	if usage1.CacheWriteTokens > 0 {
		pct := float64(usage2.CacheTokens) / float64(usage1.CacheWriteTokens) * 100
		t.Logf("Cache hit rate: wrote %d tokens on turn 1, read %d tokens on turn 2 (%.0f%%)",
			usage1.CacheWriteTokens, usage2.CacheTokens, pct)
	}

	// --- Turn 3: Another turn to verify sliding window works ---
	messages3 := append(messages2,
		llmwire.Message{
			Role:    "assistant",
			Content: resp2.Text,
		},
		llmwire.Message{
			Role:    "user",
			Content: "What is 2+2?",
		},
	)

	resp3, err := client.Chat(ctx, systemPrompt, messages3, tools)
	require.NoError(t, err)
	require.NotNil(t, resp3)
	require.NotNil(t, resp3.Usage)

	usage3 := resp3.Usage

	t.Logf("Turn 3: prompt=%d, completion=%d, cache_read=%d, cache_write=%d, response=%q",
		usage3.PromptTokens, usage3.CompletionTokens, usage3.CacheTokens, usage3.CacheWriteTokens,
		truncate(resp3.Text, 100))

	// Turn 3 should also have cache reads (even more — system + first user + conversation prefix)
	assert.Positive(t, usage3.CacheTokens,
		"third turn should read from cache")

	// Cache reads should grow as conversation grows (more prefix to cache)
	t.Logf("Cache reads progression: turn2=%d, turn3=%d", usage2.CacheTokens, usage3.CacheTokens)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

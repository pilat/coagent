package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

const thinkingResponseJSON = `{
	"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
	"content": [
		{"type": "thinking", "thinking": "let me check the file", "signature": "sig-abc"},
		{"type": "redacted_thinking", "data": "ENCRYPTED"},
		{"type": "text", "text": "reading now"},
		{"type": "tool_use", "id": "tu_1", "name": "read", "input": {"path": "/x"}}
	],
	"stop_reason": "tool_use",
	"usage": {"input_tokens": 10, "output_tokens": 5}
}`

func TestAnthropicCapturesThinkingBlocks(t *testing.T) {
	c := &anthropicClient{model: "claude-opus-5"}

	var message anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(thinkingResponseJSON), &message))

	resp, err := c.parseResponse(&message)
	require.NoError(t, err)

	assert.Equal(t, "reading now", resp.Text)
	require.Len(t, resp.ToolCalls, 1)

	payload, ok := unwrapReasoning(resp.ReasoningRaw, "claude-opus-5")
	require.True(t, ok)

	var blocks []anthropicThinkingBlock
	require.NoError(t, json.Unmarshal(payload, &blocks))
	require.Len(t, blocks, 2)
	assert.Equal(t, anthropicThinkingBlock{
		Type: "thinking", Thinking: "let me check the file", Signature: "sig-abc",
	}, blocks[0])
	assert.Equal(t, anthropicThinkingBlock{Type: "redacted_thinking", Data: "ENCRYPTED"}, blocks[1])
}

func TestAnthropicCapturesNothingWithoutThinking(t *testing.T) {
	c := &anthropicClient{model: "claude-opus-5"}

	var message anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "m", "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content": [{"type": "text", "text": "hi"}], "stop_reason": "end_turn",
		"usage": {"input_tokens": 1, "output_tokens": 1}
	}`), &message))

	resp, err := c.parseResponse(&message)
	require.NoError(t, err)
	assert.Empty(t, resp.ReasoningRaw)
}

func TestAnthropicReplaysThinkingBlocksInOrder(t *testing.T) {
	c := &anthropicClient{model: "claude-opus-5"}

	var message anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(thinkingResponseJSON), &message))

	resp, err := c.parseResponse(&message)
	require.NoError(t, err)

	converted := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      resp.Text,
		ToolCalls:    resp.ToolCalls,
		ReasoningRaw: resp.ReasoningRaw,
	}})
	require.Len(t, converted, 1)

	var sent []map[string]any
	require.NoError(t, roundTripJSON(converted[0].Content, &sent))
	require.Len(t, sent, 4)

	assert.Equal(t, "thinking", sent[0]["type"])
	assert.Equal(t, "let me check the file", sent[0]["thinking"])
	assert.Equal(t, "sig-abc", sent[0]["signature"])
	assert.Equal(t, "redacted_thinking", sent[1]["type"])
	assert.Equal(t, "ENCRYPTED", sent[1]["data"])
	assert.Equal(t, "text", sent[2]["type"], "thinking must precede text")
	assert.Equal(t, "tool_use", sent[3]["type"])
}

func TestAnthropicDropsAnotherModelsThinking(t *testing.T) {
	c := &anthropicClient{model: "claude-sonnet-4-6"}

	payload := json.RawMessage(`[{"type":"thinking","thinking":"x","signature":"s"}]`)

	converted := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      "hello",
		ReasoningRaw: wrapReasoning("claude-opus-5", payload),
	}})
	require.Len(t, converted, 1)

	var sent []map[string]any
	require.NoError(t, roundTripJSON(converted[0].Content, &sent))
	require.Len(t, sent, 1)
	assert.Equal(t, "text", sent[0]["type"])
}

func TestOpenRouterCapturesAndEchoesReasoningDetails(t *testing.T) {
	details := json.RawMessage(`[{"type":"reasoning.encrypted","data":"BLOB","id":"r1"}]`)

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       "key",
		Model:        config.ModelEntry{ID: "anthropic/claude-opus-5", MaxTokens: 64000, ContextWindow: 900_000},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	c, ok := client.(*openAICompatibleClient)
	require.True(t, ok)

	resp, err := c.parseMessage(&oaiMessage{
		Role:             roleAssistant,
		RawContent:       json.RawMessage(`"working on it"`),
		ReasoningDetails: details,
	}, "stop")
	require.NoError(t, err)

	payload, ok := unwrapReasoning(resp.ReasoningRaw, "anthropic/claude-opus-5")
	require.True(t, ok)
	assert.JSONEq(t, string(details), string(payload))

	sent := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      resp.Text,
		ReasoningRaw: resp.ReasoningRaw,
	}})
	require.Len(t, sent, 1)

	echoed, ok := sent[0]["reasoning_details"].(json.RawMessage)
	require.True(t, ok, "reasoning_details must be echoed verbatim")
	assert.JSONEq(t, string(details), string(echoed))
}

func TestOpenRouterDropsAnotherModelsReasoningDetails(t *testing.T) {
	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       "key",
		Model:        config.ModelEntry{ID: "openai/gpt-5", ContextWindow: 400_000},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	c, _ := client.(*openAICompatibleClient)

	sent := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: wrapReasoning("anthropic/claude-opus-5", json.RawMessage(`[{"type":"x"}]`)),
	}})
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0], "reasoning_details")
}

// A plain OpenAI-compatible endpoint has no reasoning_details contract; sending an
// unknown field there can 400 the request.
func TestPlainOpenAIEndpointNeverEchoesReasoningDetails(t *testing.T) {
	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "key",
		Model:   config.ModelEntry{ID: "some-model", ContextWindow: 128_000},
	})
	require.NoError(t, err)

	c, _ := client.(*openAICompatibleClient)

	sent := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: wrapReasoning("some-model", json.RawMessage(`[{"type":"x"}]`)),
	}})
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0], "reasoning_details")
}

func roundTripJSON(in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, out)
}

func TestOpenRouterReasoningGate(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		reasoning     *config.ReasoningSpec
		isOpenRouter  bool
		wantReasoning bool
		wantEffort    string
		wantKwargs    bool
	}{
		{
			name:  "openrouter reasoning model",
			model: "anthropic/claude-opus-5",
			reasoning: &config.ReasoningSpec{
				Supported: true, NativeEffort: true,
				Efforts: []string{"low", "medium", "high"},
			},
			isOpenRouter:  true,
			wantReasoning: true,
		},
		{
			name:  "a level outside the allowlist is clamped, never sent raw",
			model: "z-ai/glm-narrow",
			reasoning: &config.ReasoningSpec{
				Supported: true, NativeEffort: true,
				Efforts: []string{"high", "xhigh"},
			},
			isOpenRouter:  true,
			wantReasoning: true,
			wantEffort:    "high",
		},
		{
			name:         "a model exposing no effort selector gets no field",
			model:        "minimax/no-selector",
			reasoning:    &config.ReasoningSpec{Supported: true},
			isOpenRouter: true,
		},
		{
			name:         "openrouter model that does not reason",
			model:        "openai/gpt-5-mini",
			reasoning:    &config.ReasoningSpec{},
			isOpenRouter: true,
		},
		{
			name:         "openrouter model with no catalog spec",
			model:        "openai/gpt-5-mini",
			isOpenRouter: true,
		},
		{
			name:         "deepseek keeps its own thinking kwarg",
			model:        "deepseek-chat",
			reasoning:    &config.ReasoningSpec{Supported: true},
			isOpenRouter: true,
			wantKwargs:   true,
		},
		{
			name:      "plain openai-compatible endpoint never sees the field",
			model:     "some-model",
			reasoning: &config.ReasoningSpec{Supported: true, NativeEffort: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := captureChatRequest(t, openAICompatibleParams{
				BaseURL: "https://example.com/v1",
				APIKey:  "key",
				Model: config.ModelEntry{
					ID: tt.model, MaxTokens: 64000, ContextWindow: 200_000, Reasoning: tt.reasoning,
				},
				IsOpenRouter: tt.isOpenRouter,
			})

			if tt.wantReasoning {
				wantEffort := tt.wantEffort
				if wantEffort == "" {
					wantEffort = "medium"
				}

				require.Contains(t, body, "reasoning")
				assert.Equal(t, map[string]any{"effort": wantEffort}, body["reasoning"])
			} else {
				assert.NotContains(t, body, "reasoning")
			}

			if tt.wantKwargs {
				assert.Equal(t, map[string]any{"thinking": true}, body["chat_template_kwargs"])
			} else {
				assert.NotContains(t, body, "chat_template_kwargs")
			}
		})
	}
}

// captureChatRequest runs one Chat call against a stub endpoint and returns the
// decoded request body.
func captureChatRequest(
	t *testing.T,
	params openAICompatibleParams,
	opts ...llmwire.ChatOption,
) map[string]any {
	t.Helper()

	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, _ = w.Write(
			[]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`),
		)
	}))
	defer srv.Close()

	params.BaseURL = srv.URL

	client, err := newOpenAICompatibleClient(params)
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "sys", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "hi"},
	}, nil, opts...)
	require.NoError(t, err)

	return body
}

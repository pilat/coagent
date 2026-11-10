package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

const responsesOKBody = `{"status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant",` +
	`"content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`

func reasoningModel() config.ModelEntry {
	return config.ModelEntry{
		ID: "gpt-6-luna", MaxTokens: 8192, ContextWindow: 400_000,
		Reasoning: &config.ReasoningSpec{Supported: true, Efforts: []string{"low", "medium", "high"}},
	}
}

func newTestResponsesClient(t *testing.T, baseURL string, model config.ModelEntry) *openAIResponsesClient {
	t.Helper()

	client, err := newOpenAIResponsesClient(baseURL, "key", model)
	require.NoError(t, err)

	typed, ok := client.(*openAIResponsesClient)
	require.True(t, ok)

	return typed
}

// captureResponsesBody runs one Chat against a fake /responses endpoint and
// returns the request body exactly as it reached the wire.
func captureResponsesBody(
	t *testing.T,
	client *openAIResponsesClient,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (map[string]any, string) {
	t.Helper()

	var raw []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Equal(t, "Bearer key", r.Header.Get("Authorization"))

		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(responsesOKBody))
	}))
	t.Cleanup(server.Close)

	client.baseURL = server.URL + "/v1"

	resp, err := client.Chat(context.Background(), "be helpful", messages, tools, opts...)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Text)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))

	return body, string(raw)
}

func readTool() []llmwire.ToolSchema {
	return []llmwire.ToolSchema{
		{Name: "read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
}

func TestResponsesRequestShape(t *testing.T) {
	client := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	body, raw := captureResponsesBody(t, client, []llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}}, readTool())

	assert.Equal(t, false, body["store"], "store:false is always explicit")
	assert.Equal(t, []any{"reasoning.encrypted_content"}, body["include"])
	assert.Equal(t, "be helpful", body["instructions"], "no openAISuffix on the Responses wire")
	assert.Equal(t, "gpt-6-luna", body["model"])
	assert.InDelta(t, 8192, body["max_output_tokens"], 0)

	for _, banned := range []string{"max_tokens", "max_completion_tokens", "messages", "temperature", "stream"} {
		assert.NotContains(t, body, banned)
	}

	assert.Equal(t, "auto", body["tool_choice"])
	assert.Equal(t, []any{map[string]any{
		"type": "function", "name": "read", "description": "read a file", "strict": false,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}}, body["tools"])
	assert.Equal(t, map[string]any{"effort": "medium"}, body["reasoning"])
	assert.Equal(t, []any{map[string]any{"role": "user", "content": "hi"}}, body["input"])
	assert.NotContains(t, raw, "IMPORTANT: You MUST use the native function calling API")
}

func TestResponsesMaxOutputTokens(t *testing.T) {
	user := []llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}}

	client := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	body, _ := captureResponsesBody(t, client, user, nil, llmwire.WithMaxTokens(500))
	assert.InDelta(t, 500, body["max_output_tokens"], 0, "a per-call cap narrows the client limit")

	clamped := reasoningModel()
	clamped.MaxTokens, clamped.ContextWindow = 262144, 262144
	body, _ = captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", clamped), user, nil)
	assert.InDelta(t, 39321, body["max_output_tokens"], 0, "the window reserve clamps the catalog limit")

	unset := reasoningModel()
	unset.MaxTokens = 0
	body, _ = captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", unset), user, nil)
	assert.NotContains(t, body, "max_output_tokens")
}

func TestResponsesEffortGate(t *testing.T) {
	user := []llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}}

	clamped := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	clamped.SetReasoningLevel("xhigh")
	body, _ := captureResponsesBody(t, clamped, user, nil)
	assert.Equal(t, map[string]any{"effort": "high"}, body["reasoning"], "level clamps to the catalog allowlist")

	noEfforts := reasoningModel()
	noEfforts.Reasoning = &config.ReasoningSpec{Supported: true}
	body, _ = captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", noEfforts), user, nil)
	assert.NotContains(t, body, "reasoning")
	assert.Contains(t, body, "include", "a reasoning model still asks for encrypted reasoning")

	for name, spec := range map[string]*config.ReasoningSpec{"nil": nil, "unsupported": {Efforts: []string{"low"}}} {
		model := config.ModelEntry{ID: "gpt-4.1", MaxTokens: 8192, ContextWindow: 1_000_000, Reasoning: spec}
		body, _ = captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", model), user, nil)
		assert.NotContains(t, body, "include", name)
		assert.NotContains(t, body, "reasoning", name)
	}
}

func TestResponsesToolLessTurnKeepsCallHistory(t *testing.T) {
	client := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	body, _ := captureResponsesBody(t, client, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "go"},
		{
			Role: llmwire.RoleAssistant, Content: "looking",
			ToolCalls: []llmwire.ToolCall{{ID: "call_1", Name: "read", Arguments: []byte(`{"p":1}`)}},
		},
		{Role: llmwire.RoleTool, ToolCallID: "call_1", ToolName: "read", Content: "file body"},
		{Role: llmwire.RoleTool, ToolName: "read", Content: "legacy row"},
	}, nil)

	assert.NotContains(t, body, "tools")
	assert.NotContains(t, body, "tool_choice")
	assert.Equal(t, []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": "looking"},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": `{"p":1}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file body"},
		map[string]any{"role": "user", "content": "legacy row"},
	}, body["input"])
}

func TestResponsesToolResultImages(t *testing.T) {
	pngPath := filepath.Join(t.TempDir(), "coagent-x.png")
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	require.NoError(t, os.WriteFile(pngPath, png, 0o600))

	vision := reasoningModel()
	vision.InputModalities = []string{"text", "image"}

	body, _ := captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", vision),
		imageConversation(pngPath), nil)

	input, ok := body["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 3)
	assert.Equal(t, map[string]any{
		"type": "function_call_output", "call_id": "c1",
		"output": []any{
			map[string]any{"type": "input_text", "text": "[img]"},
			map[string]any{
				"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
			},
		},
	}, input[2])

	body, raw := captureResponsesBody(t, newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel()),
		imageConversation(pngPath), nil)
	assert.NotContains(t, raw, "input_image", "a text-only model gets no pixels")
	assert.Contains(t, raw, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision))
	assert.NotEmpty(t, body["input"])
}

func TestIsDirectOpenAI(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.openai.com/v1":           true,
		"https://API.OpenAI.com:443/v1":       true,
		"https://api.openai.com":              true,
		"https://openrouter.ai/api/v1":        false,
		"http://localhost:8000/v1":            false,
		"https://api.openai.com.evil.test/v1": false,
		"api.openai.com/v1":                   false,
		"://bad":                              false,
		"":                                    false,
	} {
		assert.Equal(t, want, isDirectOpenAI(url), url)
	}
}

func TestOpenAIDriverDispatchesOnBaseURL(t *testing.T) {
	model := config.ModelEntry{ID: "gpt-6-luna", MaxTokens: 8192, ContextWindow: 400_000}
	driver := newDrivers(nil)[driverOpenAI]

	for _, tt := range []struct {
		baseURL   string
		responses bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://API.OpenAI.com:443/v1", true},
		{"https://openrouter.ai/api/v1", false},
		{"http://localhost:8000/v1", false},
		{"https://api.openai.com.evil.test/v1", false},
		{"://bad", false},
	} {
		client, err := driver.NewClient(
			config.ProviderEntry{APIKey: "key", BaseURL: tt.baseURL},
			model,
			DriverClientOpts{},
		)
		require.NoError(t, err, tt.baseURL)

		if tt.responses {
			assert.IsType(t, &openAIResponsesClient{}, client, tt.baseURL)
			assert.Equal(t, "openai", client.Provider())

			continue
		}

		assert.IsType(t, &openAICompatibleClient{}, client, tt.baseURL)
	}

	_, err := driver.NewClient(config.ProviderEntry{APIKey: "key"}, model, DriverClientOpts{})
	require.ErrorContains(t, err, "base URL is required")
}

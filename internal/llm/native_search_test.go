package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

func orClientWithNativeSearch(t *testing.T, baseURL string, native bool) *openAICompatibleClient {
	t.Helper()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      baseURL,
		APIKey:       "key",
		Model:        config.ModelEntry{ID: "openai/gpt-5.2"},
		IsOpenRouter: true,
		NativeSearch: native,
	})
	require.NoError(t, err)

	typed, ok := client.(*openAICompatibleClient)
	require.True(t, ok)

	return typed
}

// nativeSearchTools is one client-side function tool; injection only rides
// requests that already carry tools.
func nativeSearchTools() []llmwire.ToolSchema {
	return []llmwire.ToolSchema{{
		Name:        "webfetch",
		Description: "fetches content from a URL",
		Parameters:  []byte(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
	}}
}

// The injected entry rides the outgoing body only when native search is on and
// the request carries client-side tools — and serializes without an empty
// function object (oaiToolDef.Function is a pointer for exactly this).
func TestOpenRouterNativeSearch_BodyShape(t *testing.T) {
	t.Parallel()

	type captured struct {
		body map[string]any
	}

	capture := &captured{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capture.body))
		_, _ = w.Write(
			[]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
		)
	}))
	defer srv.Close()

	t.Run("native on injects the server tool and the cap", func(t *testing.T) {
		client := orClientWithNativeSearch(t, srv.URL, true)

		_, err := client.Chat(context.Background(), "", []llmwire.Message{
			{Role: llmwire.RoleUser, Content: "hi"},
		}, nativeSearchTools())
		require.NoError(t, err)

		raw, err := json.Marshal(capture.body["tools"])
		require.NoError(t, err)
		assert.Contains(t, string(raw), "openrouter:web_search")
		assert.NotContains(t, string(raw), `"function":{}`, "server tool must not carry an empty function object")
		assert.EqualValues(t, maxNativeSearchToolCalls, capture.body["max_tool_calls"])
	})

	t.Run("native off keeps the body clean", func(t *testing.T) {
		capture.body = nil
		client := orClientWithNativeSearch(t, srv.URL, false)

		_, err := client.Chat(context.Background(), "", []llmwire.Message{
			{Role: llmwire.RoleUser, Content: "hi"},
		}, nativeSearchTools())
		require.NoError(t, err)

		raw, err := json.Marshal(capture.body["tools"])
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "openrouter:web_search")
		assert.NotContains(t, capture.body, "max_tool_calls")
	})

	t.Run("tool-less (summarizer) requests carry no injection", func(t *testing.T) {
		capture.body = nil
		client := orClientWithNativeSearch(t, srv.URL, true)

		_, err := client.Chat(context.Background(), "", []llmwire.Message{
			{Role: llmwire.RoleUser, Content: "summarize"},
		}, nil)
		require.NoError(t, err)

		assert.NotContains(t, capture.body, "max_tool_calls")
		_, hasTools := capture.body["tools"]
		assert.False(t, hasTools, "tool-less requests get no tools array at all")
	})
}

// OR-native responses carry url_citation annotations on the assistant message
// and a server_tool_use block in usage. Parsing tolerates them without
// rendering or inventing wire vocabulary.
func TestOpenRouterNativeSearch_NonStreamAnnotationTolerance(t *testing.T) {
	t.Parallel()

	body := `{
		"id":"c1","object":"chat.completion","created":1,"model":"openai/gpt-5.2",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":"OpenRouter is a gateway (see example.com).",
				"annotations":[{
					"type":"url_citation",
					"url_citation":{"url":"https://example.com/docs","title":"Docs","content":"the docs","start_index":0,"end_index":10}
				}]
			},
			"finish_reason":"stop"
		}],
		"usage":{
			"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
			"server_tool_use":{"web_search_requests":2}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := orClientWithNativeSearch(t, srv.URL, true)

	resp, err := client.Chat(context.Background(), "", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "what is this"},
	}, nativeSearchTools())
	require.NoError(t, err, "annotations must not break parsing")
	assert.Equal(t, "OpenRouter is a gateway (see example.com).", resp.Text)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 10, resp.Usage.PromptTokens)
}

// The streaming path tolerates annotations too: OR streams annotation-bearing
// deltas and the usage chunk keeps server_tool_use.
func TestOpenRouterNativeSearch_StreamAnnotationTolerance(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"grounded answer\",\"annotations\":[{\"type\":\"url_citation\",\"url_citation\":{\"url\":\"https://example.com\",\"title\":\"T\",\"content\":\"c\",\"start_index\":0,\"end_index\":5}}]}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":4,\"total_tokens\":12,\"server_tool_use\":{\"web_search_requests\":2}}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	client := orClientWithNativeSearch(t, srv.URL, true)

	resp, err := client.Chat(context.Background(), "", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "search something"},
	}, nativeSearchTools())
	require.NoError(t, err, "annotation-bearing deltas must not break the stream")
	assert.Equal(t, "grounded answer", resp.Text)
	assert.Equal(t, "stop", resp.FinishType)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 8, resp.Usage.PromptTokens)
}

// The native-search bit reaches the client only through the OR driver and
// only when config resolves it; other drivers never carry it.
func TestDriverClientOptsNativeSearchWiring(t *testing.T) {
	t.Parallel()

	entry := config.ProviderEntry{
		Driver:  driverOpenRouter,
		APIKey:  "key",
		BaseURL: "http://localhost",
	}
	model := config.ModelEntry{ID: "or-model", MaxTokens: 1024, ContextWindow: 100000}

	client, err := newDrivers(nil)[driverOpenRouter].NewClient(entry, model, DriverClientOpts{NativeSearch: true})
	require.NoError(t, err)

	typed, ok := client.(*openAICompatibleClient)
	require.True(t, ok)
	assert.True(t, typed.nativeSearch, "OR driver forwards the native-search bit")

	client, err = newDrivers(nil)[driverOpenRouter].NewClient(entry, model, DriverClientOpts{})
	require.NoError(t, err)

	typed = client.(*openAICompatibleClient)
	assert.False(t, typed.nativeSearch, "default opts keep native search off")
}

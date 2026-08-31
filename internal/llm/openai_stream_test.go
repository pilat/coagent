package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestOpenRouterChatStreamsAndAggregatesResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["stream"] != true {
			http.Error(w, "streaming required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"think \",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"first\",\"index\":0}]}}]}\n\n" +
				": OPENROUTER PROCESSING\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"again\",\"reasoning_details\":[{\"type\":\"reasoning.encrypted\",\"data\":\"sealed\",\"index\":1}],\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"ba\",\"arguments\":\"{\\\"com\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\",\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"sh\",\"arguments\":\"mand\\\":\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19,\"cost\":0.25}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Text)
	assert.Equal(t, "think again", resp.ReasoningContent)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call-1", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.JSONEq(t, `{"command":"pwd"}`, string(resp.ToolCalls[0].Arguments))
	assert.Equal(t, "tool_calls", resp.FinishType)
	assert.InDelta(t, 0.25, resp.CostUSD, 0.000001)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 12, resp.Usage.PromptTokens)
	assert.Equal(t, 7, resp.Usage.CompletionTokens)

	raw, ok := unwrapReasoning(resp.ReasoningRaw, "test-model")
	require.True(t, ok)
	assert.JSONEq(t, `[
		{"type":"reasoning.text","text":"first","index":0},
		{"type":"reasoning.encrypted","data":"sealed","index":1}
	]`, string(raw))
}

func TestOpenRouterChatSurfacesStreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"upstream timed out\",\"code\":504}}\n\n"))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream timed out")
}

func TestOpenRouterChatAggregatesTextFinish(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"ignored\"}},{\"index\":0,\"delta\":{\"content\":null}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"length\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", resp.Text)
	assert.Equal(t, "length", resp.FinishType)
	assert.Empty(t, resp.ToolCalls)
}

func TestOpenRouterChatAcceptsJSONErrorBeforeStreamStarts(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"routing failed","code":502}}`))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routing failed")
}

func TestOpenRouterChatRejectsInvalidReasoningDetails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_details\":{\"not\":\"an array\"}}}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode reasoning details")
}

func TestOpenRouterChatRejectsDoneWithoutChoice(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no streamed choices")
}

func TestOpenRouterChatRejectsIncompleteStream(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ended before [DONE]")
}

func TestOpenRouterChatCancelsStreamRead(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.Chat(ctx, "", nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

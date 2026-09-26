package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// chatWithBody runs one Chat call against a stub endpoint returning the given
// raw body and returns the resulting error.
func chatWithBody(t *testing.T, raw string) error {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL,
		APIKey:  "key",
		Model:   config.ModelEntry{ID: "test-model"},
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "sys", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "hi"},
	}, nil)

	return err
}

func TestNoChoicesBodySurfacesProviderError(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name:    "object error form",
			body:    `{"error":{"message":"upstream provider timed out","code":504}}`,
			wantMsg: "upstream provider timed out",
		},
		{
			name:    "string error form",
			body:    `{"error":"provider overloaded"}`,
			wantMsg: "provider overloaded",
		},
		{
			name:    "no error field",
			body:    `{"id":"x","choices":[]}`,
			wantMsg: "returned no choices",
		},
		{
			name:    "error message included alongside the no-choices phrase",
			body:    `{"error":{"message":"moderation flagged the request"}}`,
			wantMsg: "returned no choices: moderation flagged the request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := chatWithBody(t, tt.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}

func TestMalformedBodyReturnsDecodeError(t *testing.T) {
	err := chatWithBody(t, `{`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal response")
}

func TestTruncateBody(t *testing.T) {
	assert.Equal(t, "short", truncateBody([]byte("short")))
	assert.Equal(t, string(make([]byte, bodyLogLimit))+"...", truncateBody(make([]byte, bodyLogLimit+10)))
}

func TestConfigurationDocumentSurvivesProviderRoundTrip(t *testing.T) {
	document := "models:\n  - id: anthropic/claude-sonnet-4.6\n    provider: gateway\n" +
		"  - id: anthropic/claude-opus-4.6\n    provider: gateway\nsandbox:\n  rules:\n    - deny: ~/.ssh\n"
	arguments, err := json.Marshal(map[string]string{"document": document})
	require.NoError(t, err)
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				var received string
				for _, message := range request.Messages {
					if message.Role == "tool" {
						assert.NoError(t, json.Unmarshal(message.Content, &received))
					}
				}
				assert.Equal(t, document, received)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, fragment := range string(arguments) {
						chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
							"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
								"index": 0, "function": map[string]any{"arguments": string(fragment)},
							}}},
						}}})
						assert.NoError(t, err)
						_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
					}
					_, _ = fmt.Fprint(
						w,
						"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"edit-1\",\"function\":{\"name\":\"config_edit\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
					)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"choices": []any{
					map[string]any{
						"finish_reason": "tool_calls",
						"message": map[string]any{"role": "assistant", "tool_calls": []any{
							map[string]any{
								"id":       "edit-1",
								"type":     "function",
								"function": map[string]any{"name": "config_edit", "arguments": string(arguments)},
							},
						}},
					},
				}}))
			}))
			defer srv.Close()
			client, err := newOpenAICompatibleClient(openAICompatibleParams{
				BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: streaming,
			})
			require.NoError(t, err)
			response, err := client.Chat(context.Background(), "", []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "/config deny reads of ~/.ssh"},
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
					{ID: "read-1", Name: "read", Arguments: []byte(`{"file_path":"/fixture/config.yaml"}`)},
				}},
				{Role: llmwire.RoleTool, Content: document, ToolName: "read", ToolCallID: "read-1"},
			}, nil)
			require.NoError(t, err)
			require.Len(t, response.ToolCalls, 1)
			assert.Equal(t, arguments, response.ToolCalls[0].Arguments)
		})
	}
}

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

func TestChatToolSuffixOnlyWithTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tools  []llmwire.ToolSchema
		suffix bool
	}{
		{"no tools", nil, false},
		{"tools", []llmwire.ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				_, _ = w.Write(
					[]byte(
						`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`,
					),
				)
			}))
			defer srv.Close()

			client, err := newOpenAICompatibleClient(openAICompatibleParams{
				BaseURL: srv.URL, APIKey: "key", Model: config.ModelEntry{ID: "openai/gpt-5.6-luna"},
			})
			require.NoError(t, err)

			_, err = client.Chat(context.Background(), "sys",
				[]llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}}, tc.tools)
			require.NoError(t, err)

			require.NotEmpty(t, body.Messages)
			assert.Equal(t, "system", body.Messages[0].Role)

			if tc.suffix {
				assert.Equal(t, "sys\n\n"+openAISuffix, body.Messages[0].Content)
			} else {
				assert.Equal(t, "sys", body.Messages[0].Content)
			}
		})
	}
}

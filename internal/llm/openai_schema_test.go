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

func TestChatResponseSchema(t *testing.T) {
	schema := json.RawMessage(
		`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"],"additionalProperties":false}`,
	)
	for _, tc := range []struct {
		name                          string
		router, capable, option, want bool
	}{
		{"capable router", true, true, true, true},
		{"no option", true, true, false, false},
		{"incapable router", true, false, true, false},
		{"other endpoint", false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]json.RawMessage
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
				BaseURL: srv.URL, APIKey: "key", IsOpenRouter: tc.router,
				Model: config.ModelEntry{
					ID: "model", StructuredOutputs: tc.capable,
					OpenRouterConfig: &config.OpenRouterConfig{Order: []string{"preferred"}},
				},
			})
			require.NoError(t, err)
			var opts []llmwire.ChatOption
			if tc.option {
				opts = append(opts, llmwire.WithResponseSchema("answer", schema))
			}
			_, err = client.Chat(
				context.Background(),
				"sys",
				[]llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}},
				nil,
				opts...)
			require.NoError(t, err)
			format, exists := body["response_format"]
			require.Equal(t, tc.want, exists)
			if tc.want {
				var got oaiResponseFormat
				require.NoError(t, json.Unmarshal(format, &got))
				assert.Equal(t, "json_schema", got.Type)
				assert.Equal(t, "answer", got.JSONSchema.Name)
				assert.True(t, got.JSONSchema.Strict)
				assert.Equal(t, string(schema), string(got.JSONSchema.Schema))
				var provider map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(body["provider"], &provider))
				assert.JSONEq(t, `["preferred"]`, string(provider["order"]))
				assert.NotContains(t, provider, "require_parameters")
			}
		})
	}
}

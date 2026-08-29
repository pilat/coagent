package llm

import (
	"context"
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

func TestTruncateBody(t *testing.T) {
	assert.Equal(t, "short", truncateBody([]byte("short")))
	assert.Equal(t, string(make([]byte, bodyLogLimit))+"...", truncateBody(make([]byte, bodyLogLimit+10)))
}

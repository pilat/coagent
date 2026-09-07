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
)

func TestOpenRouterProviderPreferencesReachWire(t *testing.T) {
	t.Parallel()

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	allowFallbacks := false
	enforceDistillableText := true
	requireParameters := true
	zdr := true
	promptPrice := 1.25
	completionPrice := 2.5
	maxLatency := 8.0
	p50Throughput := 40.0
	p90Throughput := 20.0

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL,
		APIKey:  "key",
		Model: config.ModelEntry{
			ID: "test/model",
			OpenRouterConfig: &config.OpenRouterConfig{
				AllowFallbacks:         &allowFallbacks,
				DataCollection:         "deny",
				EnforceDistillableText: &enforceDistillableText,
				Ignore:                 []string{"slow-provider"},
				MaxPrice: &config.OpenRouterMaxPrice{
					Prompt: &promptPrice, Completion: &completionPrice,
				},
				Only:                   []string{"fast-provider"},
				PreferredMaxLatency:    &config.OpenRouterPreference{Value: &maxLatency},
				PreferredMinThroughput: &config.OpenRouterPreference{P50: &p50Throughput, P90: &p90Throughput},
				Quantizations:          []string{"fp8", "mxfp8"},
				RequireParameters:      &requireParameters,
				Sort:                   "latency",
				ZDR:                    &zdr,
			},
		},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.NoError(t, err)

	provider, err := json.Marshal(body["provider"])
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"allow_fallbacks": false,
		"data_collection": "deny",
		"enforce_distillable_text": true,
		"ignore": ["slow-provider"],
		"max_price": {"prompt": 1.25, "completion": 2.5},
		"only": ["fast-provider"],
		"preferred_max_latency": 8,
		"preferred_min_throughput": {"p50": 40, "p90": 20},
		"quantizations": ["fp8", "mxfp8"],
		"require_parameters": true,
		"sort": "latency",
		"zdr": true
	}`, string(provider))
}

func TestOpenRouterEmptyProviderPreferencesStayOffWire(t *testing.T) {
	t.Parallel()

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: srv.URL,
		APIKey:  "key",
		Model: config.ModelEntry{
			ID:               "test/model",
			OpenRouterConfig: &config.OpenRouterConfig{},
		},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), "", nil, nil)
	require.NoError(t, err)
	assert.NotContains(t, body, "provider")
}

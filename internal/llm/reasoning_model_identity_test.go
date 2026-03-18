package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

const identityReasoningDetails = `[{"type":"reasoning.encrypted","data":"BLOB","id":"r1"}]`

// Replay is gated on exact string equality, so the catalog's dated-vs-undated
// tolerance must stop at metadata: a rewritten id would drop every envelope.
func TestCatalogDateToleranceNeverRewritesTheConfiguredModelID(t *testing.T) {
	driver := &stubDriver{models: map[string]catalog.ModelSpec{
		"claude-sonnet-4-5-20250929": {Name: "Claude Sonnet 4.5", ContextWindow: 200_000, MaxTokens: 64_000},
	}}

	cfg := testConfig([]config.ModelEntry{{ID: "claude-sonnet-4-5", Provider: "prod"}})

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverAnthropic: driver}))

	entry := cfg.UnifiedConfig.Models[0]
	assert.Equal(t, "claude-sonnet-4-5", entry.ID, "enrichment fills metadata, never the id")
	assert.Equal(t, 200_000, entry.ContextWindow, "the dated catalog entry still resolved")
}

// The one identity the envelope rests on: the id stamped on a stored payload is
// the id that went on the wire, so a same-model replay can never be rejected and
// a foreign payload can never be accepted.
func TestEnvelopeModelIsTheWireModel(t *testing.T) {
	bodies := &requestRecorder{}
	srv := httptest.NewServer(bodies)

	defer srv.Close()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      srv.URL,
		APIKey:       "key",
		Model:        config.ModelEntry{ID: "claude-sonnet-4-5", MaxTokens: 64_000, ContextWindow: 200_000},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	ctx := context.Background()
	first := []llmwire.Message{{Role: llmwire.RoleUser, Content: "hi"}}

	resp, err := client.Chat(ctx, "sys", first, nil)
	require.NoError(t, err)

	require.Len(t, bodies.bodies, 1)

	wire, ok := bodies.bodies[0]["model"].(string)
	require.True(t, ok)
	assert.Equal(t, "claude-sonnet-4-5", wire)

	payload, ok := unwrapReasoning(resp.ReasoningRaw, wire)
	require.True(t, ok, "the envelope must unwrap for the exact id that went on the wire")
	assert.JSONEq(t, identityReasoningDetails, string(payload))

	_, err = client.Chat(ctx, "sys", append(first, llmwire.Message{
		Role:         llmwire.RoleAssistant,
		Content:      resp.Text,
		ReasoningRaw: resp.ReasoningRaw,
	}), nil)
	require.NoError(t, err)

	require.Len(t, bodies.bodies, 2)
	assert.JSONEq(t, identityReasoningDetails, assistantReasoningDetails(t, bodies.bodies[1]))
}

// The envelope's identity is the model id alone, so an id an operator repoints at
// another provider replays the old provider's payload. Anthropic's block filter
// absorbs a foreign shape; OpenRouter echoes whatever the envelope holds.
func TestEnvelopeIdentityIsTheModelIDNotTheProvider(t *testing.T) {
	const model = "shared-id"

	anthropicPayload := json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"s"}]`)
	openRouterPayload := json.RawMessage(`[{"type":"reasoning.encrypted","data":"BLOB"}]`)

	anthropicSent := (&anthropicClient{model: model}).convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: wrapReasoning(model, openRouterPayload),
	}})
	require.Len(t, anthropicSent, 1)

	var blocks []map[string]any

	require.NoError(t, roundTripJSON(anthropicSent[0].Content, &blocks))
	require.Len(t, blocks, 1)
	assert.Equal(t, "text", blocks[0]["type"], "an unrecognized block type reaches no request")

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       "key",
		Model:        config.ModelEntry{ID: model, MaxTokens: 64_000, ContextWindow: 200_000},
		IsOpenRouter: true,
	})
	require.NoError(t, err)

	c, ok := client.(*openAICompatibleClient)
	require.True(t, ok)

	openRouterSent := c.convertMessages([]llmwire.Message{{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: wrapReasoning(model, anthropicPayload),
	}})
	require.Len(t, openRouterSent, 1)
	assert.Contains(t, openRouterSent[0], "reasoning_details",
		"a matching id is the whole gate — the payload's origin provider is not checked")
}

// requestRecorder keeps every decoded chat request and answers each with a
// reasoning-bearing completion.
type requestRecorder struct {
	bodies []map[string]any
}

func (r *requestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	r.bodies = append(r.bodies, body)

	_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok",` +
		`"reasoning_details":` + identityReasoningDetails + `},"finish_reason":"stop"}],"usage":{}}`))
}

// assistantReasoningDetails returns the reasoning_details of the request's single
// assistant message, or "null" when it carries none.
func assistantReasoningDetails(t *testing.T, body map[string]any) string {
	t.Helper()

	messages, ok := body["messages"].([]any)
	require.True(t, ok, "request carries no messages")

	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != roleAssistant {
			continue
		}

		details, ok := msg["reasoning_details"]
		if !ok {
			return "null"
		}

		encoded, err := json.Marshal(details)
		require.NoError(t, err)

		return string(encoded)
	}

	require.Fail(t, "request carries no assistant message")

	return ""
}

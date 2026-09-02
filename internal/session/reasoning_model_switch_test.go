package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// A live model switch must strand the previous model's reasoning payload: replaying
// another provider's opaque blob is a hard 400 for the rest of the session. The
// whole chain is real here — provider parse, SQLite, the per-iteration reload, and
// the two clients SetModel swaps between.
func TestReasoningEnvelopeAcrossALiveModelSwitch(t *testing.T) {
	ctx := context.Background()
	provider := newReasoningProvider(t)
	sess := newModelSwitchSession(t, provider.url)

	require.NoError(t, sess.ms.addUserMessage(ctx, "task 1"))
	runModelSwitchTurn(t, sess)

	require.NoError(t, sess.ms.addUserMessage(ctx, "task 2"))
	runModelSwitchTurn(t, sess)

	require.NoError(t, sess.SetModel("model-b", ""))
	require.NoError(t, sess.ms.addUserMessage(ctx, "task 3"))
	runModelSwitchTurn(t, sess)

	require.NoError(t, sess.SetModel("model-a", ""))
	require.NoError(t, sess.ms.addUserMessage(ctx, "task 4"))
	runModelSwitchTurn(t, sess)

	require.Len(t, provider.requests, 4)

	assert.Equal(t, "model-a", provider.requests[0].model)
	assert.Empty(t, provider.requests[0].assistantReasoning, "the first turn has no history to replay")

	// Same model, one round-trip through SQLite: the payload comes back.
	assert.Equal(t, "model-a", provider.requests[1].model)
	assert.Equal(t, []string{"reasoning-1"}, provider.requests[1].assistantReasoning)

	assert.Equal(t, "model-b", provider.requests[2].model)
	assert.Empty(t, provider.requests[2].assistantReasoning,
		"model A's payloads must not reach model B")

	// Back on model A, its own payloads are legal again however stale — the envelope
	// records provenance, not freshness — while model B's stays behind.
	assert.Equal(t, "model-a", provider.requests[3].model)
	assert.Equal(t, []string{"reasoning-1", "reasoning-2"}, provider.requests[3].assistantReasoning)

	assertStoredEnvelopeModels(t, sess, []string{"model-a", "model-a", "model-b", "model-a"})
}

// runModelSwitchTurn runs one activation, which a text-only response settles after
// a single provider call.
func runModelSwitchTurn(t *testing.T, sess *svc) {
	t.Helper()

	_, err := runLoop(t.Context(), sess, loopOptions{}, iterationGuard(2))
	require.NoError(t, err)
}

// assertStoredEnvelopeModels checks the model each persisted assistant row's
// envelope names, in transcript order.
func assertStoredEnvelopeModels(t *testing.T, sess *svc, want []string) {
	t.Helper()

	stored, err := sess.store.LoadActiveMessages(context.Background(), sess.id)
	require.NoError(t, err)

	var got []string

	for _, msg := range stored {
		if len(msg.ReasoningRaw) == 0 {
			continue
		}

		var envelope struct {
			Model string `json:"model"`
		}

		require.NoError(t, json.Unmarshal(msg.ReasoningRaw, &envelope))

		got = append(got, envelope.Model)
	}

	assert.Equal(t, want, got, "every assistant row keeps the model that produced it")
}

// reasoningRequest is what one provider call carried: the model it named and the
// reasoning payload of every assistant message in it.
type reasoningRequest struct {
	model              string
	assistantReasoning []string
}

// reasoningProvider is an OpenRouter-shaped endpoint that answers every call with a
// turn-numbered reasoning payload, so a replayed one identifies its own turn.
type reasoningProvider struct {
	url      string
	requests []reasoningRequest
}

func newReasoningProvider(t *testing.T) *reasoningProvider {
	t.Helper()

	p := &reasoningProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role             string          `json:"role"`
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
			} `json:"messages"`
		}

		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		req := reasoningRequest{model: body.Model}

		for _, msg := range body.Messages {
			if msg.Role != "assistant" || len(msg.ReasoningDetails) == 0 {
				continue
			}

			var details []struct {
				Data string `json:"data"`
			}

			require.NoError(t, json.Unmarshal(msg.ReasoningDetails, &details))
			require.Len(t, details, 1)

			req.assistantReasoning = append(req.assistantReasoning, details[0].Data)
		}

		p.requests = append(p.requests, req)

		turn := len(p.requests)

		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"answer %d",`+
			`"reasoning_details":[{"type":"reasoning.encrypted","data":"reasoning-%d"}]},`+
			`"finish_reason":"stop"}],"usage":{}}`, turn, turn)
	}))
	t.Cleanup(srv.Close)

	p.url = srv.URL

	return p
}

// newModelSwitchSession wires a session over real SQLite and real OpenRouter
// clients pointed at the stub endpoint, with two models to switch between.
func newModelSwitchSession(t *testing.T, baseURL string) *svc {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)
	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "model-a", "", nil)
	require.NoError(t, err)

	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
		},
		Models: []config.ModelEntry{
			{ID: "model-a", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
			{ID: "model-b", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
		},
	}}

	client, err := llm.NewClientWithModel(cfg, "model-a")
	require.NoError(t, err)

	return &svc{
		id:              record.ID,
		rootID:          record.ID,
		cfg:             cfg,
		model:           "model-a",
		llmClient:       client,
		newLLMWithModel: llm.NewClientWithModel,
		store:           store,
		ms:              newMessageStore(store, record.ID, nil),
		loopDetector:    newLoopDetector(),
		registry:        tool.NewRegistry(),
		prompt:          newPromptBuilder(testPrompt, "", ""),
	}
}

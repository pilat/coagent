package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// TestSetModelRecordsTheEffortTheNextRunSends drives a switch to a reasoning model
// without naming a level: the model's own default is what the session runs, so it
// is what the record must carry — the record is the only thing the next run reads.
func TestSetModelRecordsTheEffortTheNextRunSends(t *testing.T) {
	provider := newEffortProvider(t)
	h := newEffortHarness(t, provider.url)

	defer h.shutdown()

	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "plain-model", nil)
	require.NoError(t, err)
	h.waitUntil("first turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 1
	})
	h.mgr.waitIdle(id)

	require.NoError(t, h.mgr.SetModel(h.ctx, id, "thinker", ""))

	rec, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"an unnamed level settles on the model's default, and the record keeps that")

	require.NoError(t, h.mgr.SendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 2
	})

	reqs := provider.snapshot()
	require.Len(t, reqs, 2)
	assert.Equal(t, "plain-model", reqs[0].model)
	assert.Empty(t, reqs[0].effort, "a model with no effort selector carries none")

	assert.Equal(t, "thinker", reqs[1].model)
	assert.Equal(t, "high", reqs[1].effort,
		"the run recreated from the record must ask for the level the switch settled on")
}

// effortRequest is what one provider call named: its model and reasoning effort.
type effortRequest struct {
	model  string
	effort string
}

// effortProvider is an OpenRouter-shaped endpoint recording the reasoning effort
// of every call, so what a session actually asks for is observable.
type effortProvider struct {
	url string

	mu       sync.Mutex
	requests []effortRequest
}

func newEffortProvider(t *testing.T) *effortProvider {
	t.Helper()

	p := &effortProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}

		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		p.mu.Lock()
		p.requests = append(p.requests, effortRequest{model: body.Model, effort: body.Reasoning.Effort})
		turn := len(p.requests)
		p.mu.Unlock()

		_, _ = fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"answer %d"},"finish_reason":"stop"}],"usage":{}}`,
			turn,
		)
	}))
	t.Cleanup(srv.Close)

	p.url = srv.URL

	return p
}

func (p *effortProvider) snapshot() []effortRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]effortRequest(nil), p.requests...)
}

// newEffortHarness is the daemon harness with real LLM clients pointed at a stub
// endpoint, and two models: one with no effort selector, one defaulting to "high".
func newEffortHarness(t *testing.T, baseURL string) *subagentHarness {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedStore := schedule.NewStore(db)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "plain-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
		},
		Models: []config.ModelEntry{
			{ID: "plain-model", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
			{
				ID: "thinker", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
				EffortLevels:  []string{"low", "high"},
				DefaultEffort: "high",
				Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: []string{"low", "high"}},
			},
		},
	}}

	factory := session.NewFactoryWithOptions(cfg, nil, nil, sessStore, nil, nil, nil, nil, nil)

	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		schedule.NewService(schedStore),
		func() string { return "plain-model" },
	)
	mgr.loadModelCatalog(cfg.UnifiedConfig.Models)

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}
}

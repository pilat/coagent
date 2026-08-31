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

// TestSpawnSettlesTheChildEffortOnTheChildModel drives a spawn onto a model whose
// effort vocabulary differs from the parent's. The parent's level is meaningless
// there, so the child must start on its own model's default — that is what its
// record has to carry, because the record is all the child's run reads.
func TestSpawnSettlesTheChildEffortOnTheChildModel(t *testing.T) {
	provider := newSpawnEffortProvider(t)
	h := newSpawnEffortHarness(t, provider.url)

	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "parent work", "parent-model", nil)
	require.NoError(t, err)
	h.waitUntil("parent answered", func() bool {
		return countAssistantReplies(h.parentMessages(parentID)) == 1
	})
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SetModel(h.ctx, parentID, "parent-model", "high"))

	child, err := h.mgr.Spawn(h.ctx, spawnRequest{
		ParentID:  parentID,
		AgentType: "general",
		Model:     "child-model",
		Prompt:    "child work",
	})
	require.NoError(t, err)
	h.waitForDelivery(child.ChildID)

	rec, err := h.sessStore.GetSession(h.ctx, child.ChildID)
	require.NoError(t, err)
	assert.Equal(t, "low", rec.ReasoningLevel,
		"the parent's level is not a level the child model offers, so the child model's default wins")

	assert.Equal(t, "low", provider.effortFor("child-model"),
		"the child must ask the provider for the level its own model defaults to")
}

// TestResolveChildEffort covers the settling rules a spawn applies before the
// child's level is persisted, including the shape a brand-new session needs
// (nothing asked for, nothing inherited).
func TestResolveChildEffort(t *testing.T) {
	entries := []config.ModelEntry{
		reasoningModelEntry("parent-model", []string{"low", "high"}, "high"),
		reasoningModelEntry("child-model", []string{"low", "medium"}, "low"),
		{ID: "plain-model", Provider: "or"},
	}

	tests := []struct {
		name      string
		entries   []config.ModelEntry
		model     string
		requested string
		inherited string
		want      string
		wantErr   string
	}{
		{
			name:    "nothing asked or inherited lands on the model default",
			entries: entries, model: "child-model", want: "low",
		},
		{
			name:    "an inherited level the model offers is kept",
			entries: entries, model: "child-model", inherited: "medium", want: "medium",
		},
		{
			name:    "an inherited level the model does not offer falls back to its default",
			entries: entries, model: "child-model", inherited: "high", want: "low",
		},
		{
			name:    "an asked-for level the model offers wins over the inherited one",
			entries: entries, model: "child-model", requested: "medium", inherited: "low", want: "medium",
		},
		{
			name:    "an asked-for level the model rejects fails the spawn",
			entries: entries, model: "child-model", requested: "high",
			wantErr: "does not accept reasoning level",
		},
		{
			name:    "a model with no effort selector carries no level",
			entries: entries, model: "plain-model", inherited: "high", want: "",
		},
		{
			name:    "an unknown model fails the spawn",
			entries: entries, model: "ghost", wantErr: "unknown model",
		},
		{
			name:  "no catalog vouches for nothing, so the level passes through",
			model: "child-model", inherited: "high", want: "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &svc{modelEntries: tt.entries}

			got, err := s.resolveChildEffort(tt.model, tt.requested, tt.inherited)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// spawnEffortProvider is an OpenRouter-shaped endpoint recording the reasoning
// effort of every call per model, so what each session asks for is observable.
type spawnEffortProvider struct {
	url string

	mu      sync.Mutex
	efforts map[string]string
}

func newSpawnEffortProvider(t *testing.T) *spawnEffortProvider {
	t.Helper()

	p := &spawnEffortProvider{efforts: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}

		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		p.mu.Lock()
		p.efforts[body.Model] = body.Reasoning.Effort
		p.mu.Unlock()

		_, _ = fmt.Fprint(w,
			`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`,
		)
	}))
	t.Cleanup(srv.Close)

	p.url = srv.URL

	return p
}

func (p *spawnEffortProvider) effortFor(model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.efforts[model]
}

// newSpawnEffortHarness is the daemon harness with real LLM clients pointed at a
// stub endpoint, and two reasoning models whose effort vocabularies overlap only
// on "low" — so an inherited level is distinguishable from a settled one.
func newSpawnEffortHarness(t *testing.T, baseURL string) *subagentHarness {
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
	cfg := &config.Config{WorkDir: workDir, Model: "parent-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
		},
		Models: []config.ModelEntry{
			reasoningModelEntry("parent-model", []string{"low", "high"}, "high"),
			reasoningModelEntry("child-model", []string{"low", "medium"}, "low"),
		},
	}}

	factory := session.NewFactoryWithOptions(cfg, nil, nil, sessStore, sessStore, nil, nil, nil, nil, nil)

	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		sessStore,
		schedule.NewService(schedStore),
		func() string { return "parent-model" },
	)
	mgr.loadModelCatalog(cfg.UnifiedConfig.Models)

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}
}

func reasoningModelEntry(id string, efforts []string, defaultEffort string) config.ModelEntry {
	return config.ModelEntry{
		ID: id, Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
		EffortLevels:  efforts,
		DefaultEffort: defaultEffort,
		Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: efforts},
	}
}

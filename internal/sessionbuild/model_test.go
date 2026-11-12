package sessionbuild

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

type levelClient struct {
	llm.Client
	level string
}

func (c *levelClient) SetReasoningLevel(level string) { c.level = level }
func (*levelClient) SetSessionID(string)              {}

func TestBuildPromptModelIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, display, stored, want string
		levels                      []string
		missing                     bool
	}{
		{"effort", "Display", "high", "Display (model), effort high", []string{"low", "high"}, false},
		{"stored fallback", "Display", "", "Display (model), effort low", []string{"low", "high"}, false},
		{"unsupported stored level", "Display", "medium", "Display (model), effort low", []string{"low", "high"}, false},
		{"narrowed catalog", "Display", "high", "Display (model), effort low", []string{"low"}, false},
		{"no levels", "Display", "medium", "Display (model)", nil, false},
		{"name fallback", "", "high", "Name (model), effort high", []string{"high"}, false},
		{"missing", "", "high", "model", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Model: "model", UnifiedConfig: &config.UnifiedConfig{}}
			if !tc.missing {
				cfg.UnifiedConfig.Models = []config.ModelEntry{
					{ID: "model", Name: "Name", DisplayName: tc.display, EffortLevels: tc.levels},
				}
				cfg.UnifiedConfig.Models[0].Reasoning = &config.ReasoningSpec{Supported: true, Efforts: tc.levels}
			}
			in := BuildInput{Config: cfg, Record: &sessionstore.SessionRecord{ID: 1, ReasoningLevel: tc.stored}}
			client := &levelClient{}
			level := configureClient(client, in.Record)
			assert.Equal(t, client.level, level)
			for _, agent := range []registry.AgentType{registry.AgentTypeBuild, registry.AgentTypeExplore, registry.AgentTypeBrowser} {
				cfg, ok := registry.NewSet(nil, registry.BrowserSettings{}).Get(agent)
				require.True(t, ok)
				prompt := buildPrompt(in, cfg, nil, level).SystemPrompt()
				assert.Contains(t, prompt, "You are running on "+tc.want+". Trust this over anything said earlier")
				if len(tc.levels) == 0 {
					assert.NotContains(t, modelSection(in.Config, "model", level), ", effort ")
				}
			}
		})
	}
}

func TestModelIdentityMatchesRequestEffort(t *testing.T) {
	for _, tc := range []struct {
		name, stored, defaultEffort, want string
		levels, allowlist                 []string
	}{
		{"missing stored level", "", "high", "low", []string{"low", "high"}, []string{"low", "high"}},
		{"unsupported stored level", "medium", "high", "low", []string{"low", "high"}, []string{"low", "high"}},
		{"narrowed catalog", "high", "low", "low", []string{"low"}, []string{"low"}},
		{"supported stored level", "high", "high", "high", []string{"low", "high"}, []string{"low", "high"}},
		{"any effort", "max", "medium", "max", []string{"low", "medium", "high"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				requests <- body.Reasoning.Effort
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`),
				)
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{Model: "model", UnifiedConfig: &config.UnifiedConfig{
				Providers: map[string]config.ProviderEntry{
					"fixture": {Driver: "openrouter", BaseURL: server.URL, APIKey: "test-key"},
				},
				Models: []config.ModelEntry{
					{
						ID:            "model",
						Provider:      "fixture",
						DisplayName:   "Display",
						EffortLevels:  tc.levels,
						DefaultEffort: tc.defaultEffort,
						Reasoning: &config.ReasoningSpec{
							Supported: true,
							Efforts:   tc.allowlist,
							AnyEffort: len(tc.allowlist) == 0,
						},
					},
				},
			}}
			client, err := llm.NewClient(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			in := BuildInput{Config: cfg, Record: &sessionstore.SessionRecord{ID: 1, ReasoningLevel: tc.stored}}
			level := configureClient(client, in.Record)
			prompt := buildPrompt(in, registry.AgentTypeConfig{}, nil, level).SystemPrompt()
			_, err = client.Chat(t.Context(), prompt, nil, nil)
			require.NoError(t, err)
			select {
			case effort := <-requests:
				assert.Equal(t, tc.want, effort)
				assert.Contains(t, prompt, "Display (model), effort "+effort+".")
			default:
				t.Fatal("no model request received")
			}
		})
	}
}

func TestBuildClientModelIdentity(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"fixture": {Driver: "openrouter", BaseURL: "http://127.0.0.1:1", APIKey: "test-key"},
		},
		Models: []config.ModelEntry{
			{
				ID:            "model",
				Provider:      "fixture",
				DisplayName:   "Display",
				EffortLevels:  []string{"low", "high"},
				DefaultEffort: "low",
			},
			{ID: "plain", Provider: "fixture"},
		},
	}}
	for _, tc := range []struct{ model, effort, want string }{
		{"model", "", "Display (model), effort low"},
		{"model", "high", "Display (model), effort high"},
		{"plain", "medium", "plain (plain)"},
	} {
		client, section, err := BuildClient(t.Context(), BuildInput{
			Config: cfg, Record: &sessionstore.SessionRecord{ID: 1, AgentType: "build"},
		}, tc.model, tc.effort)
		require.NoError(t, err)
		require.NoError(t, client.Close())
		assert.Contains(t, section.Text, "You are running on "+tc.want+".")
	}
}

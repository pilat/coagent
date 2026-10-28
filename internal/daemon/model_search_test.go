package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = modelSearchTool("")

type modelSearchTool string

type modelSearchRequest struct {
	Model string `json:"model"`
	Tools []struct {
		Type string `json:"type"`
	} `json:"tools"`
}

func TestSetModelPreservesAgentSearchPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		agent      registry.AgentType
		tool       string
		wantSearch bool
	}{
		{registry.AgentTypeAssistant, "read_session", false},
		{registry.AgentTypeBrowser, tool.PlaywrightToolPrefix + "view", false},
		{registry.AgentTypeBuild, "read_session", true},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			release := make(chan struct{})
			requests := make(chan modelSearchRequest, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request modelSearchRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}

				requests <- request
				if request.Model == "before" {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`,
					),
				)
			}))
			defer server.Close()
			defer closeOnce(release)
			h := newHarness(t, harnessOptions{})
			cfg := &config.Config{WorkDir: t.TempDir(), Model: "before", UnifiedConfig: &config.UnifiedConfig{
				Providers: map[string]config.ProviderEntry{
					"fixture": {Driver: "openrouter", BaseURL: server.URL, APIKey: "test-key"},
				},
				Models: []config.ModelEntry{
					{ID: "before", Provider: "fixture", ContextWindow: 200000, MaxTokens: 1000},
					{
						ID: "after", Provider: "fixture", ContextWindow: 200000, MaxTokens: 1000,
						EffortLevels: []string{"low", "high"}, DefaultEffort: "low",
					},
				},
			}}
			h.mgr.build.Config = cfg
			h.mgr.models = newModels(cfg)
			require.True(t, cfg.UnifiedConfig.SearchNativeActive("after"))
			project := testProject(t, h.store, cfg.WorkDir)
			record, err := h.store.CreateSession(h.ctx, project, "before", "", nil)
			require.NoError(t, err)
			_, err = h.db.ExecContext(h.ctx, `UPDATE sessions SET agent_type = ? WHERE id = ?`, tc.agent, record.ID)
			require.NoError(t, err)
			record = h.session(record.ID)
			require.Equal(t, string(tc.agent), record.AgentType)
			_, err = h.store.Enqueue(h.ctx, sessionstore.Input{
				SessionID: record.ID, Source: sessionstore.InputSourceUser, Content: "answer",
			})
			require.NoError(t, err)
			in := h.mgr.build
			in.Record, in.WorkDir = record, cfg.WorkDir
			in.OwnerTools = []tool.Tool{modelSearchTool(tc.tool)}
			sess, cleanup, err := sessionbuild.Build(h.ctx, in)
			require.NoError(t, err)
			t.Cleanup(cleanup)
			t.Cleanup(sess.Close)
			ctx, cancel := context.WithCancel(t.Context())
			rs := newRunner(cancel, cfg.WorkDir, record, waitingRunner{sessionID: record.ID}, false)
			rs.SetService(sess)
			_, registered := h.mgr.runners.register(rs)
			require.True(t, registered)
			done := make(chan struct{})
			var runErr error
			t.Cleanup(func() {
				closeOnce(release)
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("model-switch activation did not stop")
				}
				h.mgr.runners.remove(record.ID)
				rs.Complete()
			})
			go func() {
				_, runErr = sess.Run(ctx)
				close(done)
			}()
			before := nextModelSearchRequest(t, requests)
			assertModelSearchPolicy(t, before, "before", tc.wantSearch)
			require.NoError(t, h.mgr.SetModel(h.ctx, record.ID, "after", "high"))
			closeOnce(release)
			after := nextModelSearchRequest(t, requests)
			assertModelSearchPolicy(t, after, "after", tc.wantSearch)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("model-switch activation did not complete")
			}
			require.NoError(t, runErr)
			stored := h.session(record.ID)
			assert.Equal(t, "after", stored.Model)
			assert.Equal(t, "high", stored.ReasoningLevel)
			assert.True(t, cfg.UnifiedConfig.SearchNativeActive("after"), "shared configuration remains enabled")
		})
	}
}

func (t modelSearchTool) ID() string                  { return string(t) }
func (t modelSearchTool) Description() string         { return "model switch fixture" }
func (t modelSearchTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t modelSearchTool) ParallelSafe() bool          { return true }
func (t modelSearchTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	return &tool.Result{Output: "done"}, nil
}

func nextModelSearchRequest(t *testing.T, requests <-chan modelSearchRequest) modelSearchRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
		return modelSearchRequest{}
	}
}

func assertModelSearchPolicy(t *testing.T, request modelSearchRequest, model string, want bool) {
	t.Helper()
	assert.Equal(t, model, request.Model)
	assert.NotEmpty(t, request.Tools, "native search requires a nonempty function-tool inventory")
	found := false
	for _, entry := range request.Tools {
		found = found || entry.Type == "openrouter:web_search"
	}
	assert.Equal(t, want, found)
}

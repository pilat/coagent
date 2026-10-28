package sessionbuild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const assistantToolMarker = "distinctive owner read_session instance"

var _ tool.Tool = (*assistantOwnerTool)(nil)

type assistantOwnerTool struct{ runs atomic.Int64 }

type assistantBuildRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	} `json:"tools"`
}

func TestBuildAssistant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	require.NoError(
		t,
		os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("Excluded project instructions."), 0o600),
	)
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))
	store := sessionstore.NewStore(db)
	project, err := store.GetOrCreateHiddenProject(t.Context(), workDir)
	require.NoError(t, err)
	record, err := store.EnsureAssistantRoot(t.Context(), project, "manager", workDir, "fixture-model", "")
	require.NoError(t, err)
	record.Model = "fixture-model"
	memories := memory.NewCuratedStore(db)
	_, err = memories.SaveMemory(t.Context(), project, "Excluded curated memory.")
	require.NoError(t, err)
	_, err = store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: record.ID, Source: sessionstore.InputSourceUser, Content: "read a session",
	})
	require.NoError(t, err)
	requests := make(chan assistantBuildRequest, 8)
	server := assistantBuildServer(t, requests)
	t.Cleanup(server.Close)
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"fixture": {Driver: "openrouter", BaseURL: server.URL, APIKey: "test-key"},
		},
		Models: []config.ModelEntry{
			{ID: "fixture-model", Provider: "fixture", ContextWindow: 200000, MaxTokens: 1000},
		},
	}}
	require.True(t, cfg.UnifiedConfig.SearchNativeActive(record.Model))
	owner := &assistantOwnerTool{}
	head := func(context.Context) (string, error) { return "desk", nil }
	started := time.Now()
	sess, cleanup, err := Build(t.Context(), BuildInput{
		Record: record, Config: cfg, Store: store, WorkDir: workDir, MemoryStore: memories,
		OwnerTools: []tool.Tool{owner}, OutputEnabled: true, Head: head,
	})
	require.NoError(t, err)
	t.Logf("assistant Build with isolated configuration (no MCP): %s", time.Since(started))
	t.Cleanup(cleanup)
	t.Cleanup(sess.Close)
	assert.Equal(t, reflect.ValueOf(head).Pointer(), reflect.ValueOf(sess).Elem().FieldByName("head").Pointer())
	_, err = sess.Run(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, owner.runs.Load(), "the exact owner tool instance executes")
	assert.True(t, cfg.UnifiedConfig.SearchNativeActive(record.Model), "shared config is not mutated")
	close(requests)
	for request := range requests {
		assertAssistantBuildRequest(t, request)
	}
}

func (t *assistantOwnerTool) ID() string          { return "read_session" }
func (t *assistantOwnerTool) Description() string { return assistantToolMarker }
func (t *assistantOwnerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *assistantOwnerTool) ParallelSafe() bool { return false }
func (t *assistantOwnerTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	t.runs.Add(1)
	return &tool.Result{Output: assistantToolMarker}, nil
}

func assistantBuildServer(t *testing.T, requests chan<- assistantBuildRequest) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request assistantBuildRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write(
				[]byte(
					`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"read_session","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
				),
			)
			return
		}
		_, _ = w.Write(
			[]byte(
				`{"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`,
			),
		)
	}))
}

func assertAssistantBuildRequest(t *testing.T, request assistantBuildRequest) {
	t.Helper()
	require.Len(t, request.Tools, 1, "all builtin and native-search tools are excluded")
	assert.Equal(t, "read_session", request.Tools[0].Function.Name)
	assert.Equal(t, assistantToolMarker, request.Tools[0].Function.Description)
	for _, message := range request.Messages {
		assert.NotContains(t, string(message.Content), "Excluded project instructions.")
		assert.NotContains(t, string(message.Content), "Excluded curated memory.")
	}
}

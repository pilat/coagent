package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/tool"
)

type configWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
}

func TestScenario_ConfigDocumentThroughHTTPPreservesModelIDs(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "intact response", true: "damaged response rejected"}[damaged],
			func(t *testing.T) {
				testConfigDocumentThroughHTTP(t, damaged)
			},
		)
	}
}

func testConfigDocumentThroughHTTP(t *testing.T, damaged bool) {
	t.Helper()
	initial := strings.ReplaceAll(toolConfig, "claude-sonnet-5", "anthropic/claude-sonnet-4.6")
	initial = strings.ReplaceAll(initial, "claude-opus-5", "anthropic/claude-opus-4.6")
	candidate := initial + "sandbox:\n    rules:\n        - deny: ~/.ssh\n"
	if damaged {
		candidate = strings.ReplaceAll(candidate, "claude-sonnet-4.6", "")
		candidate = strings.ReplaceAll(candidate, "claude-opus-4.6", "")
	}
	configDir := newApplyConfigDirWith(t, initial)
	configPath := filepath.Join(configDir, "config.yaml")
	var sawRead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []configWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "ready"}
		finish := "stop"
		command, read, edited := false, false, false
		for _, input := range request.Messages {
			command = command || input.Role == "user" && strings.Contains(input.Content, "/config")
			if input.Role == "tool" && input.Name == "read" {
				read = true
				sawRead.Store(true)
				assert.Contains(t, input.Content, "anthropic/claude-sonnet-4.6")
				assert.Contains(t, input.Content, "anthropic/claude-opus-4.6")
			}
			edited = edited || input.Role == "tool" && input.Name == tool.IDConfigEdit
		}
		if command && !edited {
			name, args := "read", map[string]string{"file_path": configPath}
			if read {
				name, args = tool.IDConfigEdit, map[string]string{"document": candidate}
			}
			encoded, err := json.Marshal(args)
			assert.NoError(t, err)
			message["tool_calls"] = []any{map[string]any{
				"id": name + "-call", "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)},
			}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		}))
	}))
	defer server.Close()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "transport.db"), configDir, configEditRespond)
	defer d.shutdown()
	workDir, err := d.mgr.store.GetProjectWorkDir(d.ctx, d.projectID)
	require.NoError(t, err)
	wireConfig := &config.Config{Model: "fake-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{"test": {Driver: "openai", BaseURL: server.URL, APIKey: "test-key"}},
		Models:    []config.ModelEntry{{ID: "fake-model", Provider: "test", MaxTokens: 8192, ContextWindow: 100000}},
	}}
	d.mgr.build.Config = wireConfig
	d.mgr.build.WorkDir = workDir
	id := startConfigEditSession(t, d, "hello")
	d.mgr.waitIdle(id)
	assert.True(t, sawRead.Load())
	var storedDocuments []string
	for _, message := range d.parentMessages(id) {
		for _, call := range message.ToolCalls {
			if call.Name == tool.IDConfigEdit {
				var args struct {
					Document string `json:"document"`
				}
				require.NoError(t, json.Unmarshal(call.Arguments, &args))
				storedDocuments = append(storedDocuments, args.Document)
			}
		}
	}
	assert.Equal(t, []string{candidate}, storedDocuments)
	actual, err := os.ReadFile(configPath)
	require.NoError(t, err)
	if damaged {
		assert.Equal(t, initial, string(actual))
		assert.Zero(t, d.restartCount())
		assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "duplicate model id")
		return
	}
	assert.Equal(t, candidate, string(actual))
	assert.Equal(t, 1, d.restartCount())
}

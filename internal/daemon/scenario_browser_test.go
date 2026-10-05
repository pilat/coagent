package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestScenario_BrowserFramesStayStoredAndProjectToLatest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake MCP server")
	}
	imageBytes := browserScenarioPNG(t)
	encoded := base64.StdEncoding.EncodeToString(imageBytes)
	dir := t.TempDir()
	server := filepath.Join(dir, "browser.sh")
	log := filepath.Join(dir, "calls.log")
	dbPath := filepath.Join(dir, "session.db")
	workDir := t.TempDir()
	require.NoError(
		t,
		os.WriteFile(server, []byte(strings.ReplaceAll(browserScenarioScript, "IMAGE_DATA", encoded)), 0o700),
	)
	require.NoError(t, os.WriteFile(log, nil, 0o600))
	var mu sync.Mutex
	var childRequests [][]llmwire.Message
	var wireRequests []providerRequest
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "GENERAL_CHILD") {
			return textReply("general complete")
		}
		if hasUserContaining(msgs, "BROWSER_CHILD") {
			mu.Lock()
			childRequests = append(childRequests, append([]llmwire.Message(nil), msgs...))
			mu.Unlock()
			for _, step := range []struct{ id, args string }{
				{"frame-1", `{"task_state":"open page"}`},
				{"missing", `{}`},
				{"frame-2", `{"task_state":"first frame showed one item; inspect next"}`},
				{"server-error", `{"task_state":"second frame showed two items; retry after server failure","fail":true}`},
				{"frame-3", `{"task_state":"second frame showed two items; finish"}`},
			} {
				if toolResultForCallID(msgs, step.id) == nil {
					return mcpToolCall(step.id, tool.PlaywrightToolPrefix+"view", step.args)
				}
			}
			return textReply("browser complete")
		}
		if toolResultForCallID(msgs, "general-task") != nil {
			return textReply("root complete")
		}
		if toolResultForCallID(msgs, "browser-task") != nil {
			return callReply(
				"general-task",
				tool.IDTask,
				`{"prompt":"GENERAL_CHILD inspect tools","description":"inspect tools","subagent_type":"general"}`,
			)
		}
		return callReply(
			"browser-task",
			tool.IDTask,
			`{"prompt":"BROWSER_CHILD inspect example.test and report facts","description":"inspect site","subagent_type":"browser"}`,
		)
	}
	options := harnessOptions{dbPath: dbPath, respond: respond, configure: func(cfg *config.Config) {
		cfg.WorkDir = workDir
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Models: []config.ModelEntry{
				{ID: "fake-model", ContextWindow: 200000, InputModalities: []string{"text", "image"}},
			},
		}
	}, observeRequest: func(request providerRequest) {
		mu.Lock()
		wireRequests = append(wireRequests, request)
		mu.Unlock()
	}}
	h := newHarness(t, options)
	require.NoError(t, h.mgr.mcpStore.Add(h.ctx, &h.projectID, mcpstore.ServerDef{
		Name: tool.PlaywrightServerName, Command: server, Args: []string{log}, Enabled: true,
	}))
	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "inspect site", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("browser child spawned", func() bool { return h.linkByCall(rootID, "browser-task") != nil })
	childID := h.linkByCall(rootID, "browser-task").ChildID
	h.waitUntil(
		"browser child completed",
		func() bool { return lastAssistantTextDTO(h.messages(childID)) == "browser complete" },
	)
	h.waitUntil("root completed", func() bool { return lastAssistantTextDTO(h.messages(rootID)) == "root complete" })
	require.NotNil(t, h.linkByCall(rootID, "general-task"))
	generalID := h.linkByCall(rootID, "general-task").ChildID
	assert.Zero(t, countAssistantToolCallsFor(h.messages(rootID), tool.PlaywrightToolPrefix+"view"))
	stored, err := h.store.LoadActiveMessages(h.ctx, childID)
	require.NoError(t, err)
	frames := 0
	for _, row := range stored {
		if row.ToolName != tool.PlaywrightToolPrefix+"view" {
			continue
		}
		if row.ToolCallID == "missing" || row.ToolCallID == "server-error" {
			assert.True(t, row.ToolError)
			if row.ToolCallID == "missing" {
				assert.Contains(t, row.Content, "task_state")
				assert.NotContains(t, row.Content, strings.TrimSuffix(tool.UntrustedContentBegin, ">>>"))
			} else {
				assert.Contains(t, row.Content, "server failed")
				assert.Contains(t, row.Content, strings.TrimSuffix(tool.UntrustedContentBegin, ">>>"))
			}
			continue
		}
		frames++
		assert.NotContains(t, row.Content, encoded)
		assert.NotContains(t, row.Content, "browser frame replaced")
		var refs []llmwire.ImageRef
		require.NoError(t, json.Unmarshal(row.Attachments, &refs))
		require.Len(t, refs, 1)
		assert.FileExists(t, refs[0].Path)
		assert.NotEmpty(t, refs[0].Digest)
	}
	assert.Equal(t, 3, frames)
	mu.Lock()
	requests := append([][]llmwire.Message(nil), childRequests...)
	wires := append([]providerRequest(nil), wireRequests...)
	mu.Unlock()
	require.GreaterOrEqual(t, len(requests), 6)
	childWire := make([]providerRequest, 0)
	generalWire := make([]providerRequest, 0)
	rootWire := make([]providerRequest, 0)
	for _, request := range wires {
		if strings.HasSuffix(request.SessionID, ":"+strconv.FormatInt(childID, 10)) {
			childWire = append(childWire, request)
		} else if request.SessionID == strconv.FormatInt(rootID, 10) {
			rootWire = append(rootWire, request)
		} else if strings.HasSuffix(request.SessionID, ":"+strconv.FormatInt(generalID, 10)) {
			generalWire = append(generalWire, request)
		}
	}
	require.NotEmpty(t, rootWire)
	require.GreaterOrEqual(t, len(childWire), 6)
	require.NotEmpty(t, generalWire)
	assert.NotContains(t, wireToolNames(rootWire[0]), tool.PlaywrightToolPrefix+"view")
	assert.Equal(t, []string{tool.PlaywrightToolPrefix + "view"}, wireToolNames(childWire[0]))
	assert.NotContains(t, wireToolNames(generalWire[0]), tool.PlaywrightToolPrefix+"view")
	for _, schema := range rootWire[0].Tools {
		if schema.Function.Name == tool.IDTask {
			assert.Contains(t, string(schema.Function.Parameters), `"browser"`)
		}
	}
	for _, i := range []int{2, 3, 4, 5} {
		assert.Equal(t, 1, wireImageCount(childWire[i]), "step %d", i)
	}
	frameAfterServerError := toolResultForCallID(requests[4], "frame-2")
	require.NotNil(t, frameAfterServerError)
	assert.NotContains(t, frameAfterServerError.Content, "[browser frame replaced")
	assert.Contains(t, requests[4][len(requests[4])-1].Content, "server failed")
	assert.Equal(t, 4, countBrowserPlaceholders(requests[5]))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, 4, strings.Count(string(data), "call\n"), "invalid task_state must not reach the server")
	h.shutdown()
	require.NoError(t, h.db.Close())
	second := newHarness(t, options)
	second.startInboxWake()
	require.NoError(t, second.mgr.SendToChild(second.ctx, childID, "BROWSER_CHILD follow-up after restart"))
	second.waitUntil("browser follow-up after restart", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, request := range wireRequests[len(wires):] {
			if strings.HasSuffix(request.SessionID, ":"+strconv.FormatInt(childID, 10)) {
				return true
			}
		}
		return false
	})
	mu.Lock()
	var restarted providerRequest
	for _, request := range wireRequests[len(wires):] {
		if strings.HasSuffix(request.SessionID, ":"+strconv.FormatInt(childID, 10)) {
			restarted = request
			break
		}
	}
	mu.Unlock()
	assert.Equal(t, 1, wireImageCount(restarted))
	assert.Equal(t, 4, wirePlaceholderCount(restarted))
}

func TestScenario_BrowserUnavailableWithoutPlaywright(t *testing.T) {
	var mu sync.Mutex
	var requests []providerRequest
	h := newHarness(t, harnessOptions{
		respond:        func(_ string, _ []llmwire.Message) *llmwire.Response { return textReply("done") },
		observeRequest: func(request providerRequest) { mu.Lock(); requests = append(requests, request); mu.Unlock() },
	})
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "hello", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("root completed", func() bool { return lastAssistantTextDTO(h.messages(id)) == "done" })
	mu.Lock()
	captured := append([]providerRequest(nil), requests...)
	mu.Unlock()
	require.NotEmpty(t, captured)
	request := captured[0]
	for _, schema := range request.Tools {
		if schema.Function.Name == tool.IDTask {
			assert.NotContains(t, string(schema.Function.Parameters), `"browser"`)
		}
	}
}

func wireToolNames(request providerRequest) []string {
	names := make([]string, 0, len(request.Tools))
	for _, schema := range request.Tools {
		names = append(names, schema.Function.Name)
	}
	return names
}

func wireImageCount(request providerRequest) int {
	count := 0
	for _, message := range request.Messages {
		count += strings.Count(string(message.Content), `"type":"image_url"`)
	}
	return count
}

func wirePlaceholderCount(request providerRequest) int {
	count := 0
	for _, message := range request.Messages {
		count += strings.Count(string(message.Content), "[browser frame replaced")
	}
	return count
}

func countBrowserPlaceholders(messages []llmwire.Message) int {
	count := 0
	for _, message := range messages {
		if strings.Contains(message.Content, "[browser frame replaced") {
			count++
		}
	}
	return count
}

func browserScenarioPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	return out.Bytes()
}

const browserScenarioScript = `#!/bin/sh
LOG="$1"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"server/discover"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"browser","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"view","description":"View page","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      echo call >> "$LOG"
      case "$line" in
        *'"fail":true'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"isError":true,"content":[{"type":"text","text":"server failed"},{"type":"image","mimeType":"image/png","data":"IMAGE_DATA"}]}}\n' "$id" ;;
        *) printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"page frame"},{"type":"image","mimeType":"image/png","data":"IMAGE_DATA"}]}}\n' "$id" ;;
      esac
      ;;
  esac
done
`

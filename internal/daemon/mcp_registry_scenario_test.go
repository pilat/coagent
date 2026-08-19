package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// fakeMCPScript is a stdio MCP server: answers the handshake, advertises one tool
// and answers it. Every spawn and call is logged so a pooled subprocess retiring
// and respawning is observable from outside the pool.
const fakeMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
RELEASE="$3"
echo spawn >> "$LOG"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      echo call >> "$LOG"
      if [ -n "$RELEASE" ]; then
        while [ ! -f "$RELEASE" ]; do sleep 0.05; done
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
  esac
done
`

// fakeMCPServer is one on-disk stdio server plus the log that records its spawns.
type fakeMCPServer struct {
	path    string
	log     string
	pong    string
	release string
}

// newFakeMCPServer writes a server that answers with pong. A held server blocks
// every tools/call until release() is called.
func newFakeMCPServer(t *testing.T, pong string, held bool) *fakeMCPServer {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	f := &fakeMCPServer{
		path: filepath.Join(dir, "fakemcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}

	if held {
		f.release = filepath.Join(dir, "release")
	}

	require.NoError(t, os.WriteFile(f.path, []byte(fakeMCPScript), 0o700))
	require.NoError(t, os.WriteFile(f.log, nil, 0o600))

	return f
}

func (f *fakeMCPServer) addParams(name, scope string) string {
	args, err := json.Marshal([]string{f.log, f.pong, f.release})
	if err != nil {
		panic(err)
	}

	return `{"name":"` + name + `","scope":"` + scope + `","command":"` + f.path + `","args":` + string(args) + `}`
}

func (f *fakeMCPServer) unblock(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.release, nil, 0o600))
}

func (f *fakeMCPServer) count(t *testing.T, event string) int {
	t.Helper()

	data, err := os.ReadFile(f.log)
	require.NoError(t, err)

	return strings.Count(string(data), event+"\n")
}

func mcpPingCall(id string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: id, Name: "mcp__fake__ping", Arguments: []byte(`{}`),
	}}}
}

func mcpToolCall(id, name, params string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: id, Name: name, Arguments: []byte(params),
	}}}
}

func hasToolResultForCallID(msgs []llmwire.Message, callID string) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return true
		}
	}

	return false
}

func toolResultForCallID(msgs []llmwire.Message, callID string) string {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return m.Content
		}
	}

	return ""
}

// The propagation contract at the real boundary: a server registered by the
// mcp_add tool is absent from the run that registered it and present in the next
// one, spawned by the pool and answering for real.
func TestScenario_MCPAddReachesTheNextRunOnly(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "USE_IT") {
			if hasToolResultForCallID(msgs, "ping-next-run") {
				return &llmwire.Response{Text: "used it"}
			}

			return mcpPingCall("ping-next-run")
		}

		if hasToolResultForCallID(msgs, "ping-same-run") {
			return &llmwire.Response{Text: "registered"}
		}

		if hasToolResultFor(msgs, tool.IDMCPAdd) {
			return mcpPingCall("ping-same-run")
		}

		return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
	}

	h, _, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registering run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDMCPAdd), "next run")
	assert.Contains(t, toolResultForCallID(msgs, "ping-same-run"), "unknown tool",
		"the run that registered the server must not gain its tools")
	assert.Equal(t, 0, fake.count(t, "call"), "a mid-run registration executes nothing")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("next run uses the server", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used it"
	})

	msgs = h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-next-run"), "pong from fake",
		"the next run offers mcp__fake__ping and it answers from the real server")
	assert.Equal(t, 1, fake.count(t, "spawn"), "exactly one pooled subprocess served the next run")
}

// The scope override is an mcpstore contract, but what a session may call is the
// user-visible half: a project row of the same name wins in the offered tools.
func TestScenario_ProjectMCPServerOverridesTheGlobalOfTheSameName(t *testing.T) {
	global := newFakeMCPServer(t, "pong from global", false)
	project := newFakeMCPServer(t, "pong from project", false)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "USE_IT") {
			if hasToolResultForCallID(msgs, "ping-override") {
				return &llmwire.Response{Text: "used it"}
			}

			return mcpPingCall("ping-override")
		}

		if hasToolResultForCallID(msgs, "add-project") {
			return &llmwire.Response{Text: "registered both"}
		}

		if hasToolResultForCallID(msgs, "add-global") {
			return mcpToolCall("add-project", tool.IDMCPAdd, project.addParams("fake", "project"))
		}

		return mcpToolCall("add-global", tool.IDMCPAdd, global.addParams("fake", "global"))
	}

	h, _, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register both scopes", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("both registrations land", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered both"
	})

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("next run uses the server", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used it"
	})

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-override"), "pong from project")
	assert.Equal(t, 1, project.count(t, "spawn"), "the project row is the one that runs")
	assert.Equal(t, 0, global.count(t, "spawn"), "the shadowed global is never spawned")
}

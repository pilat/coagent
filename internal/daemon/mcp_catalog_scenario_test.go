package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// shiftingMCPScript logs process exit and changes tools/list when the fixture allows it.
const shiftingMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
(
  parent=$$
  while kill -0 "$parent" 2>/dev/null; do sleep 0.01; done
  echo exit >> "$LOG"
) &
echo spawn >> "$LOG"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      if [ -e "$LOG.extra" ]; then
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}},{"name":"ping2","description":"Newly available tool.","inputSchema":{"type":"object","properties":{}}}]'
      else
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]'
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}\n' "$id" "$tools"
      ;;
    *'"method":"tools/call"'*)
      echo call >> "$LOG"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func newShiftingMCPServer(t *testing.T, pong string) *fakeMCPServer {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	f := &fakeMCPServer{
		path: filepath.Join(dir, "shiftingmcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}

	require.NoError(t, os.WriteFile(f.path, []byte(shiftingMCPScript), 0o700))
	require.NoError(t, os.WriteFile(f.log, nil, 0o600))

	return f
}

// A new stack discovers fresh MCP metadata after the previous stack closed its process.
func TestScenario_NextMCPStackRediscoversChangedTools(t *testing.T) {
	fake := newShiftingMCPServer(t, "pong from fake")
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch last := lastUserText(msgs); {
		case strings.Contains(last, "USE_SECOND"):
			if hasToolResultForCallID(msgs, "ping2-new") {
				return &llmwire.Response{Text: "used second"}
			}
			return mcpToolCall("ping2-new", "mcp__fake__ping2", "{}")
		case strings.Contains(last, "USE_FIRST"):
			if hasToolResultForCallID(msgs, "ping-first") {
				return &llmwire.Response{Text: "used first"}
			}
			return mcpPingCall("ping-first")
		default:
			if hasToolResultFor(msgs, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}
			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}
	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	h.mgr.waitIdle(sessionID)
	require.Eventually(t, func() bool {
		return fake.countNoFail("exit") >= fake.countNoFail("spawn")
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_FIRST now"))
	h.waitUntil("first call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used first"
	})
	h.mgr.waitIdle(sessionID)
	firstSpawns := fake.count(t, "spawn")
	assert.GreaterOrEqual(t, firstSpawns, 1)
	assert.Equal(t, 1, fake.count(t, "call"))
	require.Eventually(t, func() bool {
		return fake.countNoFail("exit") >= fake.countNoFail("spawn")
	}, 5*time.Second, 10*time.Millisecond, "stack close must stop the MCP process")
	require.NoError(t, os.WriteFile(fake.log+".extra", nil, 0o600))

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_SECOND now"))
	h.waitUntil("second call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used second"
	})
	h.mgr.waitIdle(sessionID)
	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping2-new"), "pong from fake")
	assert.Greater(t, fake.count(t, "spawn"), firstSpawns, "the next stack must discover the changed tool list")
	assert.Equal(t, 2, fake.count(t, "call"))

	spawnsBeforeStop := fake.count(t, "spawn")
	require.NoError(t, h.mgr.Stop(h.ctx, sessionID, 0))
	assert.Equal(t, spawnsBeforeStop, fake.count(t, "spawn"),
		"transcript settlement must not launch project MCP code")
}

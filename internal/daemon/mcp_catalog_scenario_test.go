package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// shiftingMCPScript is the stdio fake server with two extra observable
// behaviors: a watcher logs "exit" when the server process dies (the pool's
// reap is thus visible), and tools/list advertises a second tool from the
// second spawn on — so a lazy reconnect discovers a changed tool list.
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
      spawns=$(grep -c '^spawn$' "$LOG")
      if [ "$spawns" -le 1 ]; then
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]'
      else
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}},{"name":"ping2","description":"Second-spawn-only tool.","inputSchema":{"type":"object","properties":{}}}]'
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

// The user-facing regression: after the pool reaps an idle MCP client, the next
// activation must still reach its first model decision without starting a
// replacement subprocess, and only the model's actual tool call starts one —
// which then answers the cached tool. The reconnect's changed tools/list never
// surfaces: the replacement simply serves the tool the snapshot advertised.
func TestScenario_ReapedMCPClientServesTheNextRunFromTheCatalog(t *testing.T) {
	fake := newShiftingMCPServer(t, "pong from fake")

	// The responder runs on session goroutines; the observation travels under
	// a mutex because the test goroutine reads it after waitUntil.
	var mu sync.Mutex
	spawnsAtCatalogCall := -1

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch last := lastUserText(msgs); {
		case strings.Contains(last, "USE_THIRD"):
			if !hasToolResultForCallID(msgs, "ping2-catalog") {
				return mcpToolCall("ping2-catalog", "mcp__fake__ping2", `{}`)
			}

			return &llmwire.Response{Text: "tried the extra tool"}
		case strings.Contains(last, "USE_AGAIN"):
			if !hasToolResultForCallID(msgs, "ping-catalog") {
				mu.Lock()
				spawnsAtCatalogCall = fake.countNoFail("spawn")
				mu.Unlock()

				return mcpPingCall("ping-catalog")
			}

			return &llmwire.Response{Text: "used again"}
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(msgs, "ping-cold") {
				return &llmwire.Response{Text: "used it"}
			}

			return mcpToolCall("ping-cold", "mcp__fake__ping", `{}`)
		default:
			if hasToolResultFor(msgs, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}

	// A 50ms idle TTL reaps the client shortly after the first run releases it.
	h, _, _ := newMCPHarnessWithIdleTTL(t, respond, 50*time.Millisecond)
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	// The registering loop must be gone before USE_IT lands: a live runner
	// serves queued input from its own run, whose session predates the new
	// server — the ping call would then resolve against no such tool.
	h.mgr.waitIdle(sessionID)

	// The registering run's tail iteration may have built a session after the
	// registration and cold-started a client for it. Reap every spawned client
	// before USE_IT so the first measured run deterministically starts exactly
	// one subprocess itself — whether from a catalog hit or a cold discovery —
	// instead of racing the reaper's 50ms TTL.
	require.Eventually(t, func() bool {
		return fake.count(t, "exit") >= fake.count(t, "spawn")
	}, 5*time.Second, 10*time.Millisecond, "the registering run's clients were reaped")
	spawnBase := fake.count(t, "spawn")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("cold run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used it"
	})
	h.mgr.waitIdle(sessionID)
	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, spawnBase+1, fake.count(t, "spawn"), "the first run performs exactly one cold discovery")
	assert.Equal(t, 1, fake.count(t, "call"))

	// The run released its stack; the pool's short TTL reaps the client while
	// the catalog of discovered metadata stays. Wait until every spawned
	// client is reaped (not just one): otherwise the next run would join a
	// still-live client instead of starting its own replacement subprocess.
	require.Eventually(t, func() bool {
		return fake.count(t, "exit") >= fake.count(t, "spawn")
	}, 5*time.Second, 10*time.Millisecond, "the idle client was reaped")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_AGAIN please"))
	h.waitUntil("catalog run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used again"
	})

	msgs = h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-catalog"), "pong from fake",
		"the lazily reconnected subprocess answers the cached tool")

	mu.Lock()
	spawns := spawnsAtCatalogCall
	mu.Unlock()
	assert.Equal(t, spawnBase+1, spawns,
		"the run that issues the cached tool call has not started a replacement subprocess yet")

	assert.Equal(t, spawnBase+2, fake.count(t, "spawn"),
		"the model's tool call starts exactly one replacement subprocess")
	assert.Equal(t, 2, fake.count(t, "call"), "both runs executed the ping tool once")

	// The boundary guard against catalog pollution: the reconnect observed a
	// tools/list with ping2, but the daemon catalog must still describe only
	// the originally discovered tool set. A third catalog-hit run therefore
	// knows nothing about ping2 — its call resolves to "unknown tool" without
	// starting any process.
	require.Eventually(t, func() bool {
		return fake.countNoFail("exit") >= fake.countNoFail("spawn")
	}, 5*time.Second, 10*time.Millisecond, "the replacement client idled out and was reaped")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_THIRD now"))
	h.waitUntil("third run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "tried the extra tool"
	})

	msgs = h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping2-catalog"), "unknown tool",
		"the reconnect's changed tools/list must not have leaked into the daemon catalog")
	assert.Equal(t, spawnBase+2, fake.count(t, "spawn"),
		"a polluted catalog would have spawned to serve ping2")
	assert.Equal(t, 2, fake.count(t, "call"),
		"the server must never have received a ping2 call")
}

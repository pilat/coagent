package daemon

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// identityMCPScript answers with its PID: spawn counts span sessions, a PID names one session's process.
const identityMCPScript = `#!/bin/bash
LOG="$1"
PONG="$2"
RELEASE="$3"
echo spawn >> "$LOG"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"identity","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      if [ -p "$RELEASE" ]; then
        exec 3<> "$RELEASE"
        echo call >> "$LOG"
        read -r released <&3 || exit 1
      else
        echo call >> "$LOG"
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s pid=%s"}]}}\n' "$id" "$PONG" "$$"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

const (
	toggledPing   = "mcp__toggled__ping"
	neighbourPing = "mcp__neighbour__ping"
	toggledParams = `{"name":"toggled","scope":"project"}`
)

var identityPID = regexp.MustCompile(`pid=(\d+)`)

type toggleCall struct{ id, name, params string }

type toggleTurn struct {
	marker string
	calls  []toggleCall
	answer string
}

type toggleFixture struct {
	*harness
	toggled, neighbour *fakeMCPServer
	answering          chan struct{}
	proceed            chan struct{}
	gate, released     sync.Once
	gated              bool
}

func TestScenario_MCPToggleRestartsAllSessionServersInTheNextRun(t *testing.T) {
	flows := []struct {
		name        string
		held, gated bool
		toggle      func(t *testing.T, f *toggleFixture, holder int64)
		wantRestart bool
	}{
		{
			name: "a run started while off", held: true, wantRestart: true,
			toggle: func(t *testing.T, f *toggleFixture, holder int64) {
				f.holdThroughDisable(t, holder)
				f.waitAnswer(holder, "held run done")
				f.say(t, holder, "ENABLE_IT again", "enabled")
			},
		},
		{
			name: "no run started while off", held: true, gated: true, wantRestart: true,
			toggle: func(t *testing.T, f *toggleFixture, holder int64) {
				f.holdThroughDisable(t, holder)
				require.True(t, closedInTime(f.answering), "the held run never reached its answer")
				require.NoError(t, f.mgr.sendToSession(f.ctx, holder, "ENABLE_IT again"))
				f.release()
				f.waitAnswer(holder, "enabled")
			},
		},
		{
			name: "another session toggles while this one is idle", wantRestart: true,
			toggle: func(t *testing.T, f *toggleFixture, _ int64) {
				other := f.start(t, "DISABLE_IT now", "disabled")
				f.say(t, other, "ENABLE_IT again", "enabled")
			},
		},
		{
			name: "one run switches it off and on", wantRestart: true,
			toggle: func(t *testing.T, f *toggleFixture, holder int64) {
				f.say(t, holder, "BOUNCE_IT", "bounced")
			},
		},
		{
			name: "enabling an enabled server changes nothing",
			toggle: func(t *testing.T, f *toggleFixture, holder int64) {
				f.say(t, holder, "ENABLE_IT again", "enabled")
			},
		},
		{name: "nobody toggles", toggle: func(*testing.T, *toggleFixture, int64) {}},
	}
	for _, tt := range flows {
		t.Run(tt.name, func(t *testing.T) {
			f := newToggleFixture(t, tt.held, tt.gated)
			holder := f.start(t, "REGISTER both servers", "registered")
			if tt.held {
				f.say(t, holder, "USE_NEIGHBOUR now", "used neighbour")
			} else {
				f.say(t, holder, "USE_BEFORE now", "used before")
			}
			neighbourBefore := f.pidOf(t, holder, "before-neighbour")

			tt.toggle(t, f, holder)

			toggledBefore := f.pidOf(t, holder, "before-toggled")
			f.say(t, holder, "USE_AFTER now", "used after")
			msgs := f.messages(holder)
			require.NoError(t, llmtest.ValidateToolPairing(msgs))
			assert.Contains(t, toolResultForCallID(msgs, "before-toggled").Content, "pong from toggled",
				"the call in flight across the toggle answers normally")
			if tt.wantRestart {
				assert.NotEqual(t, toggledBefore, f.pidOf(t, holder, "after-toggled"),
					"the toggled server comes up fresh in the next run")
			} else {
				assert.Equal(t, toggledBefore, f.pidOf(t, holder, "after-toggled"),
					"an untoggled server keeps its process")
			}
			if tt.wantRestart {
				assert.NotEqual(t, neighbourBefore, f.pidOf(t, holder, "after-neighbour"),
					"a toggle deliberately restarts the neighbour in the same session")
			} else {
				assert.Equal(t, neighbourBefore, f.pidOf(t, holder, "after-neighbour"))
			}
		})
	}
}

func newToggleFixture(t *testing.T, held, gated bool) *toggleFixture {
	t.Helper()
	f := &toggleFixture{
		toggled:   newIdentityMCPServer(t, "pong from toggled", held),
		neighbour: newIdentityMCPServer(t, "pong from neighbour", false),
		answering: make(chan struct{}),
		proceed:   make(chan struct{}),
		gated:     gated,
	}
	f.harness = newHarness(t, harnessOptions{respond: f.respond})
	t.Cleanup(f.release)
	f.startInboxWake()
	return f
}

func (f *toggleFixture) release() { f.released.Do(func() { close(f.proceed) }) }

// closedInTime keeps a broken flow from stranding the test or the session's goroutine.
func closedInTime(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func newIdentityMCPServer(t *testing.T, pong string, held bool) *fakeMCPServer {
	t.Helper()
	fake := newFakeMCPServer(t, pong, held)
	require.NoError(t, os.WriteFile(fake.path, []byte(identityMCPScript), 0o700))
	if held {
		require.NoError(t, syscall.Mkfifo(fake.release, 0o600))
	}
	return fake
}

func (f *toggleFixture) turns() []toggleTurn {
	return []toggleTurn{
		{"REGISTER", []toggleCall{
			{"add-toggled", tool.IDMCPAdd, f.toggled.addParams("toggled", "project")},
			{"add-neighbour", tool.IDMCPAdd, f.neighbour.addParams("neighbour", "project")},
		}, "registered"},
		{"USE_NEIGHBOUR", []toggleCall{{"before-neighbour", neighbourPing, `{}`}}, "used neighbour"},
		{"USE_BEFORE", []toggleCall{
			{"before-toggled", toggledPing, `{}`},
			{"before-neighbour", neighbourPing, `{}`},
		}, "used before"},
		{"HOLD_IT", []toggleCall{{"before-toggled", toggledPing, `{}`}}, "held run done"},
		{"DISABLE_IT", []toggleCall{{"disable-1", tool.IDMCPDisable, toggledParams}}, "disabled"},
		{"ENABLE_IT", []toggleCall{{"enable-1", tool.IDMCPEnable, toggledParams}}, "enabled"},
		{"BOUNCE_IT", []toggleCall{
			{"bounce-off", tool.IDMCPDisable, toggledParams},
			{"bounce-on", tool.IDMCPEnable, toggledParams},
		}, "bounced"},
		{"USE_AFTER", []toggleCall{
			{"after-toggled", toggledPing, `{}`},
			{"after-neighbour", neighbourPing, `{}`},
		}, "used after"},
	}
}

func (f *toggleFixture) respond(_ string, msgs []llmwire.Message) *llmwire.Response {
	last := lastUserText(msgs)
	for _, turn := range f.turns() {
		if !strings.Contains(last, turn.marker) {
			continue
		}
		for _, call := range turn.calls {
			if toolResultForCallID(msgs, call.id) == nil {
				return callReply(call.id, call.name, call.params)
			}
		}
		// New input enqueued here precedes the next boundary, so this run must accept it.
		if turn.marker == "HOLD_IT" && f.gated {
			f.gate.Do(func() { close(f.answering) })
			if !closedInTime(f.proceed) {
				return textReply("held answer gate timed out")
			}
		}
		return textReply(turn.answer)
	}
	return textReply("unscripted")
}

func (f *toggleFixture) start(t *testing.T, prompt, answer string) int64 {
	t.Helper()
	id, err := f.mgr.Send(f.ctx, f.projectID, prompt, "fake-model", nil)
	require.NoError(t, err)
	f.waitAnswer(id, answer)
	return id
}

func (f *toggleFixture) say(t *testing.T, id int64, prompt, answer string) {
	t.Helper()
	require.NoError(t, f.mgr.sendToSession(f.ctx, id, prompt))
	f.waitAnswer(id, answer)
}

func (f *toggleFixture) waitAnswer(id int64, answer string) {
	f.t.Helper()
	f.waitUntil(answer, func() bool { return lastAssistantTextDTO(f.messages(id)) == answer })
	f.waitUntil("session idle", func() bool { return !f.mgr.HasActiveLoop(id) })
}

func (f *toggleFixture) pidOf(t *testing.T, id int64, callID string) int {
	t.Helper()
	result := toolResultForCallID(f.messages(id), callID)
	require.NotNil(t, result, callID)
	match := identityPID.FindStringSubmatch(result.Content)
	require.NotNil(t, match, result.Content)
	pid, err := strconv.Atoi(match[1])
	require.NoError(t, err)
	return pid
}

func (f *toggleFixture) holdThroughDisable(t *testing.T, holder int64) {
	t.Helper()
	require.NoError(t, f.mgr.sendToSession(f.ctx, holder, "HOLD_IT while I reconfigure"))
	f.waitUntil("the held call reaches the server", func() bool { return f.toggled.count(t, "call") == 1 })
	f.start(t, "DISABLE_IT now", "disabled")
	assert.True(t, f.mgr.HasActiveLoop(holder), "a registry change must not tear down the active call")
	release, err := os.OpenFile(f.toggled.release, os.O_RDWR, 0o600)
	require.NoError(t, err)
	_, err = release.WriteString("release\n")
	require.NoError(t, err)
	require.NoError(t, release.Close())
	require.NoError(t, os.Remove(f.toggled.release))
}

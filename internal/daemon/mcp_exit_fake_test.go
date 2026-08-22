package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const exitTrackingMCPScript = `#!/bin/sh
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
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"exit-mcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
  esac
done
`

type exitTrackingMCPServer struct {
	path string
	log  string
	pong string
}

func newExitTrackingMCPServer(t *testing.T, pong string) *exitTrackingMCPServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	fake := &exitTrackingMCPServer{
		path: filepath.Join(dir, "exit-mcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}
	require.NoError(t, os.WriteFile(fake.path, []byte(exitTrackingMCPScript), 0o700))
	require.NoError(t, os.WriteFile(fake.log, nil, 0o600))

	return fake
}

func (f *exitTrackingMCPServer) args() []string {
	return []string{f.log, f.pong}
}

func (f *exitTrackingMCPServer) count(t *testing.T, event string) int {
	t.Helper()
	data, err := os.ReadFile(f.log)
	require.NoError(t, err)

	return strings.Count(string(data), event+"\n")
}

func (f *exitTrackingMCPServer) waitForExit(t *testing.T) {
	f.waitForExitCount(t, 1)
}

func (f *exitTrackingMCPServer) waitForExitCount(t *testing.T, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.count(t, "exit") >= want
	}, 5*time.Second, 10*time.Millisecond, "MCP subprocesses did not exit")
}

func exitTrackingParams(t *testing.T, fake *exitTrackingMCPServer) string {
	t.Helper()
	data, err := json.Marshal(fake.args())
	require.NoError(t, err)

	return `{"name":"fake","scope":"project","command":"` + fake.path + `","args":` + string(data) + `}`
}

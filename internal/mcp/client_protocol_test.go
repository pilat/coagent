package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacySpecMCPScript models a spec-compliant legacy stdio server: unknown
// methods get the JSON-RPC MethodNotFound error, then the classic handshake.
const legacySpecMCPScript = `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"legacy","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

// modernMCPScript models a 2026-07-28 stdio server: stateless, no initialize —
// it answers server/discover and serves tools with per-request _meta.
const modernMCPScript = `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"server/discover"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong"}]}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func writeMCPServerScript(t *testing.T, script string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "server.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))

	return path
}

// A spec-compliant legacy server must answer the client's server/discover probe
// with MethodNotFound and fall back instantly; an ignore-style server would pay
// the probe timeout on every pooled connect.
func TestNewClient_LegacyServerAnswersProbeAndFallsBackFast(t *testing.T) {
	start := time.Now()

	client, err := NewClient(context.Background(), "legacy", ServerConfig{
		Command: "sh", Args: []string{writeMCPServerScript(t, legacySpecMCPScript)},
	}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.Less(t, time.Since(start), 3*time.Second, "probe fallback must not wait out the discover timeout")
	require.Contains(t, client.Tools(), "ping")
}

// A modern (2026-07-28) server skips the initialize handshake entirely: the
// probe succeeds and every request carries protocol _meta instead.
func TestNewClient_ModernServerSkipsHandshake(t *testing.T) {
	client, err := NewClient(context.Background(), "modern", ServerConfig{
		Command: "sh", Args: []string{writeMCPServerScript(t, modernMCPScript)},
	}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.Contains(t, client.Tools(), "ping")

	output, err := client.CallTool(context.Background(), "ping", nil)
	require.NoError(t, err)
	require.Equal(t, "pong", output)
}

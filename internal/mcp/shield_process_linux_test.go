//go:build linux

package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
)

func TestShieldedMCPProcessCannotReadOrWriteOutsideProject(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	secret := filepath.Join(outside, "secret")
	outsideWrite := filepath.Join(outside, "written")
	projectWrite := filepath.Join(project, "started")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))

	server := filepath.Join(project, "fake-mcp")
	require.NoError(t, os.WriteFile(server, []byte(`#!/bin/sh
if cat "$DENIED_READ" >/dev/null 2>&1; then exit 41; fi
if touch "$DENIED_WRITE" 2>/dev/null; then exit 42; fi
printf started > "$PROJECT_WRITE"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}}\n' "$id" ;;
    *'"method":"tools/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id" ;;
  esac
done
`), 0o700))

	runner, err := bashsandbox.New(bashsandbox.Config{
		Enabled: true, WorkDir: project, SessionKey: "mcp-test", ReadScope: bashsandbox.ProjectConfined,
	}, nil)
	require.NoError(t, err)
	client, err := NewClient(t.Context(), "fake", ServerConfig{
		Command: server, WorkDir: project,
		Env: map[string]string{
			"DENIED_READ": secret, "DENIED_WRITE": outsideWrite, "PROJECT_WRITE": projectWrite,
		},
	}, nil, runner)
	require.NoError(t, err)
	require.NoError(t, client.Close())
	assert.FileExists(t, projectWrite)
	assert.NoFileExists(t, outsideWrite)
}

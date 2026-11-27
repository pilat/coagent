package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const skillsServerScript = `#!/bin/sh
dir=$1
modern=$2
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\("[^"]*"\|[0-9][0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  printf '%s\n' "$method" >> "$dir/calls.log"
  result=''
  error=''
  case "$method" in
    server/discover)
      if [ "$modern" = true ]; then
        result="{\"supportedVersions\":[\"2026-07-28\"],\"capabilities\":$(cat "$dir/capabilities.json")}"
      else error='{"code":-32601,"message":"Method not found"}'; fi ;;
    initialize)
      result="{\"protocolVersion\":\"2024-11-05\",\"serverInfo\":{\"name\":\"fixture\",\"version\":\"1\"},\"capabilities\":$(cat "$dir/capabilities.json")}" ;;
    tools/list) result="{\"tools\":$(cat "$dir/tools.json")}" ;;
    skills/list)
      if [ -f "$dir/skills-error" ]; then error='{"code":-32603,"message":"fixture failure"}'
      elif [ "$modern" = true ] && ! printf '%s' "$line" | grep -q '"io.modelcontextprotocol/protocolVersion":"2026-07-28"'; then
        error='{"code":-32602,"message":"missing protocol metadata"}'
      else
        cursor=$(printf '%s' "$line" | sed -n 's/.*"cursor":"\([^"]*\)".*/\1/p')
        if [ -n "$cursor" ]; then result=$(cat "$dir/skills-$cursor.json"); else result=$(cat "$dir/skills.json"); fi
      fi ;;
    resources/read)
      uri=$(printf '%s' "$line" | sed -n 's/.*"uri":"\([^"]*\)".*/\1/p')
      key=$(printf '%s' "$uri" | sha256sum | cut -c 1-16)
      if [ -f "$dir/read/$key.json" ]; then result=$(cat "$dir/read/$key.json"); else error='{"code":-32602,"message":"missing fixture"}'; fi ;;
    *) error='{"code":-32601,"message":"Method not found"}' ;;
  esac
  if [ -n "$error" ]; then printf '{"jsonrpc":"2.0","id":%s,"error":%s}\n' "$id" "$error"
  else printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$result"; fi
done
`

func writeSkillsServer(t *testing.T, dir string, modern bool) string {
	t.Helper()

	path := filepath.Join(dir, "server.sh")
	require.NoError(t, os.WriteFile(path, []byte(skillsServerScript), 0o600))

	return path
}

func writeSkillFixture(t *testing.T, dir, name string, value any) {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0o600))
}

func skillEntry(name string) map[string]any {
	uri := "skill://" + name + "/SKILL.md"

	return map[string]any{
		"uri":         uri,
		"frontmatter": map[string]any{"name": name, "description": "Use " + name},
		"resources": []any{
			map[string]any{"uri": uri, "size": 3, "digest": fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("abc")))},
		},
	}
}

func skillsFixture(t *testing.T, declared, modern bool) (string, ServerConfig) {
	t.Helper()

	dir := t.TempDir()
	capabilities := map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}
	if declared {
		capabilities["extensions"] = map[string]any{"io.modelcontextprotocol/skills": map[string]any{}}
	}

	writeSkillFixture(t, dir, "capabilities.json", capabilities)
	writeSkillFixture(
		t,
		dir,
		"tools.json",
		[]any{map[string]any{"name": "view", "inputSchema": map[string]any{"type": "object"}}},
	)
	writeSkillFixture(t, dir, "skills.json", map[string]any{"skills": []any{skillEntry("pdf")}})

	return dir, ServerConfig{Command: "sh", Args: []string{writeSkillsServer(t, dir, modern), dir, fmt.Sprint(modern)}}
}

func skillsClient(t *testing.T, declared, modern bool) (*Client, string) {
	t.Helper()

	dir, cfg := skillsFixture(t, declared, modern)
	c, err := NewClient(t.Context(), "docs", cfg, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	return c, dir
}

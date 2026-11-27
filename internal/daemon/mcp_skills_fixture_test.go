package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
)

const mcpSkillsScenarioScript = `#!/bin/sh
dir=$1
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\("[^"]*"\|[0-9][0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  printf '%s\n' "$method" >> "$dir/calls.log"
  result=''
  error=''
  case "$method" in
    server/discover) error='{"code":-32601,"message":"Method not found"}' ;;
    initialize)
      result="{\"protocolVersion\":\"2024-11-05\",\"serverInfo\":{\"name\":\"fixture\",\"version\":\"1\"},\"capabilities\":{\"tools\":{},\"resources\":{},\"extensions\":{\"io.modelcontextprotocol/skills\":{}}}}" ;;
    tools/list) result='{"tools":[{"name":"view","inputSchema":{"type":"object"}}]}' ;;
    skills/list) result=$(cat "$dir/skills.json") ;;
    resources/read)
      uri=$(printf '%s' "$line" | sed -n 's/.*"uri":"\([^"]*\)".*/\1/p')
      printf 'read %s\n' "$uri" >> "$dir/calls.log"
      key=$(printf '%s' "$uri" | sha256sum | cut -c 1-16)
      if [ -f "$dir/read/$key.json" ]; then result=$(cat "$dir/read/$key.json"); else error='{"code":-32602,"message":"missing fixture"}'; fi ;;
    *) error='{"code":-32601,"message":"Method not found"}' ;;
  esac
  if [ -n "$error" ]; then printf '{"jsonrpc":"2.0","id":%s,"error":%s}\n' "$id" "$error"
  else printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$result"; fi
done
`

func newMCPSkillsHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*skillHarness, string) {
	t.Helper()

	workDir := t.TempDir()
	recorder := &skillRecorder{}
	h := newHarness(t, harnessOptions{
		respond: recorder.wrap(respond),
		configure: func(cfg *config.Config) {
			cfg.WorkDir = workDir
		},
	})
	dir := writeMCPSkillsScenarioServer(t)
	require.NoError(t, h.mgr.mcpStore.Add(h.ctx, &h.projectID, mcpstore.ServerDef{
		Name: "docs", Command: "sh", Args: []string{filepath.Join(dir, "server.sh"), dir}, Enabled: true,
	}))

	return &skillHarness{harness: h, recorder: recorder}, dir
}

func writeMCPSkillsScenarioServer(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.sh"), []byte(mcpSkillsScenarioScript), 0o600))
	files := map[string]string{
		"skill://guide/SKILL.md":         skillDoc("guide", "The guide", "Follow the guide for $ARGUMENTS."),
		"skill://guide/scripts/hello.sh": "#!/bin/sh\necho HELLO_FROM_MCP_SKILL\n",
	}
	var resources []map[string]any
	for uri, text := range files {
		resources = append(resources, map[string]any{
			"uri": uri, "size": len(text), "digest": fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(text))),
		})
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(uri)))[:16]
		writeMCPSkillsScenarioJSON(t, filepath.Join(dir, "read", key+".json"), map[string]any{
			"contents": []map[string]any{{"uri": uri, "text": text}},
		})
	}
	writeMCPSkillsScenarioJSON(t, filepath.Join(dir, "skills.json"), map[string]any{
		"skills": []map[string]any{{
			"uri": "skill://guide/SKILL.md", "frontmatter": map[string]any{"name": "guide", "description": "The guide"},
			"resources": resources,
		}},
	})

	return dir
}

func writeMCPSkillsScenarioJSON(t *testing.T, path string, value any) {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

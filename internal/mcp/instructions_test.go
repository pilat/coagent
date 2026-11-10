package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

const instructionsMCPScript = `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"0.0.1"},"instructions":%s}}\n' "$id" "$1"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"view","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func TestClientInstructions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		want     string
		warnings int
	}{
		{"absent", "", "", 0},
		{"short", "Use the latest frame.\nText: 世界", "Use the latest frame.\nText: 世界", 0},
		{"at limit", strings.Repeat("界", 8000), strings.Repeat("界", 8000), 0},
		{"over limit", strings.Repeat("界", 8000) + "終", strings.Repeat("界", 8000) + "\n[guidance truncated by coagent at 8000 characters]", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.text)
			require.NoError(t, err)
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			client, err := NewClient(ctx, "remote", ServerConfig{
				Command: "sh", Args: []string{writeMCPServerScript(t, instructionsMCPScript), string(raw)},
			}, nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			assert.Equal(t, tc.want, client.Instructions())
			assert.True(t, utf8.ValidString(client.Instructions()))
			service := &svc{clients: map[string]*Client{"remote": client}}
			for range 2 {
				reg := tool.NewRegistry()
				assert.Equal(t, 1, service.RegisterTools(reg, nil, nil, nil))
				declaration := reg.Get("mcp__remote__view").(tool.PromptDeclarer).PromptDeclaration()
				assert.Equal(t, "remote", declaration.InstructionsSource)
				assert.Equal(t, tc.want, declaration.Instructions)
				require.NoError(t, client.refreshTools(ctx))
			}
			assert.Equal(t, tc.warnings, logs.FilterMessage("mcp_instructions_truncated").Len())
			assert.Equal(t, tc.warnings, logs.Len())
		})
	}
}

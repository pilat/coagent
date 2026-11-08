package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestRenderToolTrail_ArgumentsAndNames(t *testing.T) {
	reg := trailRegistry(t)
	require.NotNil(t, reg.Get("memory_save"))

	for _, tt := range []struct{ name, args, want string }{
		{"bash", `{"command":"go test * ~~x~~"}`, "→ bash: `go test * ~~x~~`"},
		{"read", `{"file_path":"/project/src/main.go"}`, "→ read: `src/main.go`"},
		{"write", `{"file_path":"/project/new.go"}`, "→ write: `new.go`"},
		{"edit", `{"file_path":"/project/main.go"}`, "→ edit: `main.go`"},
		{"grep", `{"pattern":"func main"}`, "→ grep: `func main`"},
		{"glob", `{"pattern":"**/*.go"}`, "→ glob: `**/*.go`"},
		{"ls", `{"path":"/project"}`, "→ ls: `.`"},
		{"webfetch", `{"url":"https://example.test"}`, "→ webfetch: `https://example.test`"},
		{"websearch", `{"query":"Go context"}`, "→ websearch: `Go context`"},
		{"task", `{"description":"inspect store"}`, "→ task: `inspect store`"},
		{"skill", `{"name":"review"}`, "→ skill: `review`"},
		{"read", `{"file_path":"/outside/main.go"}`, "→ read: `/outside/main.go`"},
		{"read", `{"file_path":"/project-other/main.go"}`, "→ read: `/project-other/main.go`"},
		{"read", `{"file_path":"/project/../main.go"}`, "→ read: `/project/../main.go`"},
		{"read", `{"file_path":"/project/..cache/file"}`, "→ read: `..cache/file`"},
		{"ls", `{"path":"src"}`, "→ ls: `src`"},
		{"bash", `{"command":"\t go\n test\r\n ./...  "}`, "→ bash: `go test ./...`"},
		{"bash", "{\"command\":\"echo `date`\"}", "→ bash: `echo 'date'`"},
		{"bash", `{}`, "→ bash"},
		{"bash", `{"command":42}`, "→ bash"},
		{"bash", `{"command":null}`, "→ bash"},
		{"bash", `{"command":" \n\t"}`, "→ bash"},
		{"bash", `{`, "→ bash"},
		{"mcp__docs__search", `{"query":"hidden"}`, "→ mcp__docs__search"},
		{"unknown", `{"command":"hidden"}`, "→ unknown"},
		{"apply_patch", `{"patch":"hidden"}`, "→ apply_patch"},
		{"todowrite", `{}`, "→ todowrite"},
		{"memory_save", `{"text":"hidden"}`, "→ memory_save"},
		{"lsp", `{}`, "→ lsp"},
		{"tail", `{}`, "→ tail"},
		{"sleep", `{}`, "→ sleep"},
		{"schedule", `{}`, "→ schedule"},
		{"bad\n`name\r", `{}`, "→ badname"},
		{strings.Repeat("界", 81), `{}`, "→ " + strings.Repeat("界", 79) + "…"},
	} {
		t.Run(tt.name+tt.args, func(t *testing.T) {
			t.Parallel()
			assert.Equal(
				t,
				tt.want,
				renderToolTrail([]llmwire.ToolCall{{Name: tt.name, Arguments: []byte(tt.args)}}, reg, "/project"),
			)
		})
	}
}

func TestRenderToolTrail_BatchOrderAndFallback(t *testing.T) {
	reg := trailRegistry(t)

	for _, tt := range []struct{ name, args, want string }{
		{
			"expanded", `{"calls":[{"tool":"read","params":{"file_path":"/project/a.go"}},{"tool":"grep","params":{"pattern":"abc"}}]}`,
			"→ read: `a.go`\n→ grep: `abc`",
		},
		{
			"nested", `{"calls":[{"tool":"batch","params":{"calls":[{"tool":"read"}]}},{"params":{}},{"tool":"  ","params":{}}]}`,
			"→ batch\n→ ?\n→ ?",
		},
		{"invalid JSON", `{`, "→ batch"},
		{"invalid calls", `{"calls":42}`, "→ batch"},
		{"missing calls", `{}`, "→ batch"},
		{"empty calls", `{"calls":[]}`, "→ batch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want+"\n→ sleep", renderToolTrail([]llmwire.ToolCall{
				{Name: "batch", Arguments: []byte(tt.args)}, {Name: "sleep", Arguments: []byte(`{}`)},
			}, reg, "/project"))
		})
	}
}

func TestRenderToolTrail_ClipsRunesAfterRedaction(t *testing.T) {
	reg := trailRegistry(t)
	secret := "trail-test-registered-secret"
	logger.SetRedactedValues([]string{secret})
	t.Cleanup(func() { logger.SetRedactedValues(nil) })
	for _, value := range []string{strings.Repeat("界", 80), strings.Repeat("界", 81), strings.Repeat("界", 74) + secret} {
		args, err := json.Marshal(map[string]string{"command": value})
		require.NoError(t, err)
		want := strings.Repeat("界", 80)
		if len([]rune(value)) > 80 {
			want = strings.Repeat("界", 79) + "…"
		}
		if strings.Contains(value, secret) {
			want = strings.Repeat("界", 74) + "[REDA…"
		}
		assert.Equal(
			t,
			"→ bash: `"+want+"`",
			renderToolTrail([]llmwire.ToolCall{{Name: "bash", Arguments: args}}, reg, "/project"),
		)
	}
}

func TestRenderToolTrail_UnregisteredToolShowsOnlyItsName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, args, want string }{
		{"read", `{"file_path":"/project/main.go"}`, "→ read"},
		{"websearch", `{"query":"Go context"}`, "→ websearch"},
		{"task", `{"description":"inspect store"}`, "→ task: `inspect store`"},
		{"batch", `{"calls":[{"tool":"grep","params":{"pattern":"abc"}}]}`, "→ grep"},
	} {
		assert.Equal(
			t,
			tt.want,
			renderToolTrail(
				[]llmwire.ToolCall{{Name: tt.name, Arguments: []byte(tt.args)}},
				tool.NewRegistry(),
				"/project",
			),
		)
	}
}

func TestRenderToolTrail_NilRegistryShowsOnlyItsName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "→ read", renderToolTrail(
		[]llmwire.ToolCall{{Name: "read", Arguments: []byte(`{"file_path":"/project/main.go"}`)}},
		nil,
		"/project",
	))
}

func trailRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	unified := &config.UnifiedConfig{}
	unified.Tools.Search = config.SearchToolConfig{Provider: config.SearchProviderTavily, APIKey: "tvly-test"}
	stack, err := builtin.BuildStack(t.Context(), builtin.StackConfig{
		WorkDir: t.TempDir(), Loader: loader.New(), Todo: todo.New(), Unified: unified,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	stack.Registry.Register(builtin.NewMemorySaveTool(nil, 1))

	return stack.Registry
}

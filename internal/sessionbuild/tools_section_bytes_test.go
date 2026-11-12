package sessionbuild

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const fakeToolListMCPScript = `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"},"instructions":%s}}\n' "$id" "$2"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}\n' "$id" "$1"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

var (
	updateToolsSection = flag.Bool(
		"update-tools-section",
		false,
		"rewrite testdata/tools_section",
	)
	toolsSectionFiles                           = make(map[string]bool)
	_                 backgroundprocess.Service = bytesProcessService{}
)

type bytesMemoryStore struct{ memory.CuratedStore }

type bytesProcessService struct{}

func TestMain(m *testing.M) {
	code := m.Run()
	if *updateToolsSection {
		if err := checkToolsSectionFiles(filepath.Join("testdata", "tools_section"), toolsSectionFiles); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

func TestToolsSectionBytes_AgentTypes(t *testing.T) {
	reg, frames := realRegistry(t, true, nil, nil)
	set := registry.NewSet(nil, registry.BrowserSettings{})
	for _, agent := range []registry.AgentType{registry.AgentTypeBuild, registry.AgentTypeGeneral, registry.AgentTypeExplore} {
		cfg, ok := set.Get(agent)
		require.True(t, ok)
		filtered := filterRegistryForAgent(set, reg, cfg, frames.Owns)
		assertToolsSectionGolden(t, "agent-"+string(agent), renderToolsSection(filtered, false))
	}
	assertToolsSectionGolden(t, "all-tools", renderToolsSection(reg, false))
}

func TestToolsSectionBytes_EachToolAlone(t *testing.T) {
	reg, _ := realRegistry(t, true, nil, nil)
	for _, id := range reg.IDs() {
		assertToolsSectionGolden(t, "alone-"+id, renderToolsSection(reg.Filter([]string{id}), false))
	}
}

func TestToolsSectionBytes_Search(t *testing.T) {
	withSearch, _ := realRegistry(t, true, nil, nil)
	noSearch, _ := realRegistry(t, false, nil, nil)
	assertToolsSectionGolden(t, "no-websearch-native", renderToolsSection(noSearch, true))
	assertToolsSectionGolden(t, "no-websearch-no-native", renderToolsSection(noSearch, false))
	assertToolsSectionGolden(t, "websearch-native", renderToolsSection(withSearch, true))
	assertToolsSectionGolden(
		t,
		"websearch-without-webfetch",
		renderToolsSection(withSearch.Filter([]string{"websearch"}), false),
	)
	assertToolsSectionGolden(t, "native-only-read", renderToolsSection(noSearch.Filter([]string{"read"}), true))
	assertToolsSectionGolden(t, "native-toolless", renderToolsSection(tool.NewRegistry(), true))
}

func TestToolsSectionBytes_MCP(t *testing.T) {
	servers := mcpServerFixture(t, map[string][]string{
		"tavily":       {"tavily_search", "tavily_extract"},
		"brave-search": {"brave_web_search", "brave_local"},
		"exa":          {"search", "fetch"},
		"exa__nested":  {"search"},
		"kagi":         {"kagi_search", "summarize"},
		"searxng":      {"searxng_web_search", "web_url_read"},
		"duckduckgo":   {"duckduckgo_search"},
		"perplexity":   {"perplexity_search", "perplexity_ask"},
		"firecrawl":    {"firecrawl_search", "firecrawl_scrape"},
		"context7":     {"query-docs", "search_docs"},
		"Tavily":       {"tavily_search"},
		"playwright":   {"browser_snapshot", "browser_click"},
	}, nil)
	reg, frames := realRegistry(t, true, servers, &config.BrowserConfig{MCPName: "playwright"})
	set := registry.NewSet(nil, registry.BrowserSettings{})
	browser, ok := set.Get(registry.AgentTypeBrowser)
	require.True(t, ok)
	assertToolsSectionGolden(t, "agent-browser", renderToolsSection(
		filterRegistryForAgent(set, reg, browser, frames.Owns), false,
	))
	assertToolsSectionGolden(t, "mcp-search-order", renderToolsSection(reg, false))
	assertToolsSectionGolden(t, "mcp-unknown-server-search-word-only",
		renderToolsSection(reg.Filter([]string{"mcp__context7__search_docs"}), false))
	assertToolsSectionGolden(t, "mcp-non-search-tool-of-known-server",
		renderToolsSection(reg.Filter([]string{"mcp__tavily__tavily_extract"}), false))
	assertToolsSectionGolden(t, "mcp-any-tool-of-server-named-search",
		renderToolsSection(reg.Filter([]string{"mcp__brave-search__brave_local", "webfetch"}), false))
	assertToolsSectionGolden(t, "mcp-one-search-tool",
		renderToolsSection(reg.Filter([]string{"mcp__tavily__tavily_search"}), true))
}

func TestToolsSectionBytes_GoldenInventory(t *testing.T) {
	dir := t.TempDir()
	written := map[string]bool{"current.txt": true}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "current.txt"), []byte("current"), 0o600))
	require.NoError(t, checkToolsSectionFiles(dir, written))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("stale"), 0o600))
	require.ErrorContains(t, checkToolsSectionFiles(dir, written), "stale.txt")
}

func (bytesProcessService) Store() backgroundprocess.Store { return nil }

func (bytesProcessService) RemoveOutput(context.Context, string) error { return nil }

func (bytesProcessService) Start(
	context.Context, backgroundprocess.Spec, func(context.Context) (*exec.Cmd, error),
) (backgroundprocess.Process, error) {
	return backgroundprocess.Process{}, nil
}

func (bytesProcessService) Advertise(context.Context, string) (bool, error) { return false, nil }

func (bytesProcessService) CancelProcess(context.Context, string, backgroundprocess.HostIntent) (int, error) {
	return 0, nil
}

func (bytesProcessService) CancelTree(context.Context, int64, backgroundprocess.HostIntent) (int, error) {
	return 0, nil
}

func (bytesProcessService) CancelSessions(context.Context, []int64, backgroundprocess.HostIntent) (int, error) {
	return 0, nil
}

func (bytesProcessService) CancelAll(context.Context, backgroundprocess.HostIntent) (int, error) {
	return 0, nil
}

func (bytesProcessService) InterruptNonterminal(context.Context) (int, error) { return 0, nil }

func coreForeverStubs() []tool.Tool {
	ids := []string{"task", "send_to_subagent", "get_subagent_result", "schedule", "sleep"}
	stubs := make([]tool.Tool, 0, len(ids))
	for _, id := range ids {
		stubs = append(stubs, testTool{id: id})
	}
	return stubs
}

func mcpServerFixture(
	t *testing.T,
	servers map[string][]string,
	guidance map[string]string,
) map[string]mcp.ServerConfig {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fakemcp.sh")
	require.NoError(t, os.WriteFile(script, []byte(fakeToolListMCPScript), 0o700))
	out := make(map[string]mcp.ServerConfig, len(servers))
	for name, tools := range servers {
		list := make([]map[string]any, 0, len(tools))
		for _, tl := range tools {
			list = append(list, map[string]any{
				"name": tl, "description": "fixture",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			})
		}
		raw, err := json.Marshal(list)
		require.NoError(t, err)
		instructions, err := json.Marshal(guidance[name])
		require.NoError(t, err)
		out[name] = mcp.ServerConfig{Command: script, Args: []string{string(raw), string(instructions)}}
	}
	return out
}

func realRegistry(
	t *testing.T,
	search bool,
	servers map[string]mcp.ServerConfig,
	browser *config.BrowserConfig,
) (*tool.Registry, tool.FrameRule) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	unified := &config.UnifiedConfig{Browser: browser}
	if search {
		unified.Tools.Search = config.SearchToolConfig{Provider: config.SearchProviderTavily, APIKey: "tvly-test"}
	}
	ldr := loader.New()
	stack, err := builtin.BuildStack(t.Context(), builtin.StackConfig{
		WorkDir: t.TempDir(), Loader: ldr, Todo: todo.New(), Unified: unified, Servers: servers,
		ProcessService: bytesProcessService{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	reg := stack.Registry
	reg.Register(builtin.NewMemorySaveTool(bytesMemoryStore{}, 1))
	reg.Register(builtin.NewMemoryDeleteTool(bytesMemoryStore{}, 1))
	for _, stub := range coreForeverStubs() {
		reg.Register(stub)
	}
	return reg, stack.Frames()
}

func renderToolsSection(reg *tool.Registry, native bool) string {
	prompt := sessionprompt.NewBuilder("", "")
	prompt.SetNativeSearch(native)
	prompt.RefreshToolsSection(reg)
	return prompt.SystemPrompt()
}

func assertToolsSectionGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "tools_section", name+".txt")
	if *updateToolsSection {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
		toolsSectionFiles[filepath.Base(path)] = true
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got, name)
}

func checkToolsSectionFiles(dir string, written map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read tools section goldens: %w", err)
	}
	for _, entry := range entries {
		if !written[entry.Name()] {
			return fmt.Errorf("stale tools section golden: %s", filepath.Join(dir, entry.Name()))
		}
	}
	return nil
}

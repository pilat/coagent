package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedConfig_Browser(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		data string
		want *BrowserConfig
	}{
		{"absent", "", nil},
		{"configured", "browser:\n  mcp_name: remote\n", &BrowserConfig{MCPName: "remote"}},
		{"explicit over merged null", "<<: {browser: ~}\nbrowser:\n  mcp_name: remote\n", &BrowserConfig{MCPName: "remote"}},
		{"first merge source wins", "<<: [{browser: {mcp_name: remote}}, {browser: ~}]\n", &BrowserConfig{MCPName: "remote"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseUnifiedConfig([]byte(tt.data))
			require.NoError(t, err)
			assert.Equal(t, tt.want, parsed.Browser)
			resolved, err := ParseAndResolve([]byte(tt.data), nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, resolved.Browser)
			raw, err := MarshalUnifiedConfig(resolved)
			require.NoError(t, err)
			roundTripped, err := ParseAndResolve(raw, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, roundTripped.Browser)
		})
	}
}

const browserModelsYAML = "providers:\n  ant:\n    driver: anthropic\n    api_key: test\n" +
	"models:\n  - id: cheap\n    provider: ant\n"

func TestUnifiedConfig_BrowserModelEffortTools(t *testing.T) {
	t.Parallel()

	data := browserModelsYAML +
		"browser:\n  mcp_name: remote\n  model: cheap\n  effort: low\n  tools: [read, write, nope]\n"
	want := &BrowserConfig{MCPName: "remote", Model: "cheap", Effort: "low", Tools: []string{"read", "write", "nope"}}

	parsed, err := ParseUnifiedConfig([]byte(data))
	require.NoError(t, err)
	assert.Equal(t, want, parsed.Browser)
	resolved, err := ParseAndResolve([]byte(data), nil)
	require.NoError(t, err)
	assert.Equal(t, want, resolved.Browser)
	raw, err := MarshalUnifiedConfig(resolved)
	require.NoError(t, err)
	roundTripped, err := ParseAndResolve(raw, nil)
	require.NoError(t, err)
	assert.Equal(t, want, roundTripped.Browser)
}

func TestUnifiedConfig_RejectsInvalidBrowserModel(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ data, want string }{
		{"browser:\n  mcp_name: remote\n  effort: low\n", "browser.effort requires browser.model"},
		{"browser:\n  mcp_name: remote\n  model: other\n", `browser.model "other" is not an id in models`},
		{"browser:\n  mcp_name: remote\n  model: ' cheap'\n", `browser.model " cheap" is not an id in models`},
		{"browser:\n  model: cheap\n  tools: [read]\n", "browser.mcp_name must be non-empty"},
	} {
		t.Run(tt.data, func(t *testing.T) {
			t.Parallel()
			data := browserModelsYAML + tt.data
			_, err := LoadUnifiedConfig(writeConfig(t, data), nil)
			require.ErrorContains(t, err, tt.want)
			_, err = ParseAndResolve([]byte(data), nil)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestUnifiedConfig_RejectsInvalidBrowser(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		"browser:\n",
		"browser: ~\n",
		"browser: null\n",
		"browser:\n  # mcp_name: remote\n",
		"browser: {}\n",
		"browser:\n  mcp_name: ''\n",
		"browser:\n  mcp_name: ' remote '\n",
		"browser:\n  mcp_name: 'remote '\n",
		"browser:\n  mcp_name: ' remote'\n",
		"tools: {search: &n ~}\nbrowser: *n\n",
		"<<: {browser: ~}\n",
		"<<: [{sandbox: {}}, {browser: ~}]\n",
		"<<: [{browser: ~}, {browser: {mcp_name: remote}}]\n",
		"<<: {<<: {browser: {mcp_name: remote}}, browser: ~}\n",
	} {
		t.Run(data, func(t *testing.T) {
			t.Parallel()
			_, err := LoadUnifiedConfig(writeConfig(t, data), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "browser")
			_, err = ParseAndResolve([]byte(data), nil)
			require.Error(t, err)
		})
	}
}

func TestUnifiedConfig_BrowserKeepsStrictParsing(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		"browser:\n  mcp_server: remote\n",
		"browser:\n  mcp_name: remote\nunknown: true\n",
	} {
		_, err := ParseUnifiedConfig([]byte(data))
		require.ErrorContains(t, err, "not found in type")
	}
}

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

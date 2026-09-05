package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedConfig_SearchValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		searchYAML  string
		wantErr     string
		wantClamped int
	}{
		{
			name:       "tavily with key loads",
			searchYAML: "provider: tavily\napi_key: tvly-test\n",
		},
		{
			name:        "tavily max_results clamps",
			searchYAML:  "provider: tavily\napi_key: tvly-test\nmax_results: 99\n",
			wantClamped: 10,
		},
		{
			name:       "tavily without key fails",
			searchYAML: "provider: tavily\n",
			wantErr:    `tools.search (provider: tavily) requires "api_key"`,
		},
		{
			name:       "searxng with base_url loads",
			searchYAML: "provider: searxng\nbase_url: https://searx.example.com\n",
		},
		{
			name:       "searxng without base_url fails",
			searchYAML: "provider: searxng\n",
			wantErr:    `tools.search (provider: searxng) requires "base_url"`,
		},
		{
			name:       "unknown provider fails naming the field",
			searchYAML: "provider: bing\napi_key: x\n",
			wantErr:    `tools.search has unknown provider "bing"`,
		},
		{
			name:       "enabled false is inert with empty provider",
			searchYAML: "enabled: false\n",
		},
		{
			name:       "enabled false skips provider requirements",
			searchYAML: "provider: tavily\nenabled: false\n",
		},
		{
			name:       "enabled true without provider fails",
			searchYAML: "enabled: true\n",
			wantErr:    `tools.search requires "provider"`,
		},
		{
			name:       "empty search section is unconfigured",
			searchYAML: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, `
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
tools:
  search:
`+indentTwoLevels(tt.searchYAML))

			cfg, err := LoadUnifiedConfig(path, nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			if tt.wantClamped != 0 {
				assert.Equal(t, tt.wantClamped, cfg.Tools.Search.MaxResults)
			}
		})
	}
}

// indentTwoLevels indents every non-empty line, nesting the fragments under
// the `tools.search:` key of the surrounding fixture.
func indentTwoLevels(s string) string {
	var out strings.Builder

	for line := range strings.SplitSeq(strings.TrimPrefix(s, "\n"), "\n") {
		if line != "" {
			line = "    " + line
		}

		out.WriteString(line)
		out.WriteString("\n")
	}

	return out.String()
}

func TestUnifiedConfig_SearchMaxResultsDefault(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
tools:
  search:
    provider: tavily
    api_key: tvly-test
`)

	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.Equal(t, 5, cfg.Tools.Search.MaxResults)
}

func TestUnifiedConfig_SearchSecretExpansionAndRedaction(t *testing.T) {
	t.Parallel()

	secrets := Secrets{"TAVILY_API_KEY": "expanded-value-from-secrets"}

	cfg, err := ParseAndResolve([]byte(`
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
tools:
  search:
    provider: tavily
    api_key: ${TAVILY_API_KEY}
`), secrets)
	require.NoError(t, err)
	assert.Equal(t, "expanded-value-from-secrets", cfg.Tools.Search.APIKey)

	values := SecretValues(secrets, cfg)
	assert.Contains(t, values, "expanded-value-from-secrets")
}

func TestUnifiedConfig_SearchUndefinedSecretReferenceFailsLoad(t *testing.T) {
	t.Parallel()

	_, err := ParseAndResolve([]byte(`
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
tools:
  search:
    provider: tavily
    api_key: ${MISSING_TAVILY_KEY}
`), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_TAVILY_KEY")
}

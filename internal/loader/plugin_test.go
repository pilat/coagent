package loader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePluginManifest(t *testing.T) {
	content := `{
  "name": "acme",
  "version": "0.3.1",
  "description": "Battle-tested skills for professional workflows",
  "author": {
    "name": "Test Author",
    "email": "test@example.invalid"
  },
  "keywords": ["brainstormer", "planning", "implement"],
  "agents": ["./humanizer.md"]
}`

	manifest, err := parsePluginManifest(content)
	require.NoError(t, err)
	require.NotNil(t, manifest)

	assert.Equal(t, "acme", manifest.Name)
	assert.Equal(t, "0.3.1", manifest.Version)
	assert.Equal(t, "Battle-tested skills for professional workflows", manifest.Description)
	assert.Equal(t, "Test Author", manifest.Author.Name)
	assert.Equal(t, "test@example.invalid", manifest.Author.Email)
	assert.Equal(t, []string{"brainstormer", "planning", "implement"}, manifest.Keywords)
	assert.Equal(t, []string{"./humanizer.md"}, manifest.Agents)
}

func TestParsePluginManifest_Minimal(t *testing.T) {
	content := `{
  "name": "minimal-plugin",
  "version": "1.0.0"
}`

	manifest, err := parsePluginManifest(content)
	require.NoError(t, err)
	require.NotNil(t, manifest)

	assert.Equal(t, "minimal-plugin", manifest.Name)
	assert.Equal(t, "1.0.0", manifest.Version)
	assert.Empty(t, manifest.Description)
}

func TestParsePluginManifest_InvalidJSON(t *testing.T) {
	content := `{invalid json`
	manifest, err := parsePluginManifest(content)
	require.Error(t, err)
	assert.Nil(t, manifest)
}

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			name:      "standard github.com URL",
			url:       "github.com/example/acme-marketplace",
			wantOwner: "example",
			wantRepo:  "acme-marketplace",
		},
		{
			name:      "URL with https prefix",
			url:       "https://github.com/user/repo",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "URL with http prefix",
			url:       "http://github.com/user/repo",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "URL with trailing slash",
			url:       "github.com/user/repo/",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{name: "invalid URL - no repo", url: "github.com/user", wantErr: true},
		{name: "invalid URL - empty", url: "", wantErr: true},
		{name: "invalid URL - wrong domain", url: "gitlab.com/user/repo", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestCacheDirForMarketplace(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	cacheDir, err := cacheDirForMarketplace("example", "acme-marketplace")
	require.NoError(t, err)

	assert.Contains(t, cacheDir, "example")
	assert.Contains(t, cacheDir, "acme-marketplace")
}

func TestCacheDirForMarketplace_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	cacheDir, err := cacheDirForMarketplace("example", "acme-marketplace")
	require.Error(t, err)
	assert.Empty(t, cacheDir)
}

//go:build integration

package loader

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	coagentgit "github.com/pilat/coagent/internal/git"
)

const (
	fixtureMarketplaceURL = "github.com/example/acme-marketplace"
	fixturePluginName     = "test-plugin"
)

type localRepoGitClient struct {
	delegate      coagentgit.Client
	sourceDir     string
	cloneCalls    int
	pullCalls     int
	lastRemoteURL string
}

func (c *localRepoGitClient) Clone(ctx context.Context, remoteURL, destPath string) error {
	c.cloneCalls++
	c.lastRemoteURL = remoteURL

	return c.delegate.Clone(ctx, c.sourceDir, destPath)
}

func (c *localRepoGitClient) Pull(ctx context.Context, repoPath string) error {
	c.pullCalls++

	return c.delegate.Pull(ctx, repoPath)
}

func (c *localRepoGitClient) IsCloned(ctx context.Context, repoPath string) bool {
	return c.delegate.IsCloned(ctx, repoPath)
}

func (c *localRepoGitClient) GetRemoteURL(ctx context.Context, repoPath string) (string, error) {
	return c.delegate.GetRemoteURL(ctx, repoPath)
}

func TestIntegration_Marketplace_LocalGitRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	sourceDir := newLocalMarketplaceRepository(t)
	gitClient := &localRepoGitClient{delegate: coagentgit.New(), sourceDir: sourceDir}
	service := New(NewMarketplaceCache(gitClient)).(*svc)
	entry := fixtureMarketplaceEntry()

	require.NoError(t, service.ProcessMarketplace(context.Background(), entry, nil))
	assert.Len(t, service.marketplaceSkillPaths, 1)
	assert.Len(t, service.marketplaceAgentPaths, 1)
	assert.Equal(t, 1, gitClient.cloneCalls)
	assert.Equal(t, "https://"+fixtureMarketplaceURL, gitClient.lastRemoteURL)

	workDir := t.TempDir()
	require.NoError(t, service.LoadSkills(workDir))
	require.NoError(t, service.LoadSubagents(workDir))
	assert.NotNil(t, service.GetSkill(fixturePluginName+":test-skill"))
	assert.NotNil(t, service.GetSubagent(fixturePluginName+":test-agent"))

	cacheDir := filepath.Join(
		home,
		coagenthome.DirName,
		coagenthome.CacheDirName,
		coagenthome.MarketplacesDirName,
		"example",
		"acme-marketplace",
	)
	assert.DirExists(t, cacheDir)
}

func TestIntegration_Marketplace_DiskCachePullsLocalRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("HOME", t.TempDir())
	sourceDir := newLocalMarketplaceRepository(t)
	gitClient := &localRepoGitClient{delegate: coagentgit.New(), sourceDir: sourceDir}
	entry := fixtureMarketplaceEntry()

	first := New(NewMarketplaceCache(gitClient)).(*svc)
	require.NoError(t, first.ProcessMarketplace(context.Background(), entry, nil))
	assert.Equal(t, 1, gitClient.cloneCalls)

	addFixtureSkill(t, sourceDir, "second-skill")
	commitLocalRepository(t, sourceDir, "add second skill")

	second := New(NewMarketplaceCache(gitClient)).(*svc)
	require.NoError(t, second.ProcessMarketplace(context.Background(), entry, nil))
	require.NoError(t, second.LoadSkills(t.TempDir()))
	assert.NotNil(t, second.GetSkill(fixturePluginName+":second-skill"))
	assert.Equal(t, 1, gitClient.cloneCalls, "disk cache must prevent a second clone")
	assert.Equal(t, 1, gitClient.pullCalls, "a fresh in-memory cache must refresh the disk clone")
}

func fixtureMarketplaceEntry() config.MarketplaceEntry {
	return config.MarketplaceEntry{URL: fixtureMarketplaceURL, Plugins: []string{fixturePluginName}}
}

func newLocalMarketplaceRepository(t *testing.T) string {
	t.Helper()

	dir := createMockMarketplaceRepo(t, "example", "acme-marketplace", map[string]mockPlugin{
		fixturePluginName: {
			manifest: pluginManifest{Name: fixturePluginName, Version: "1.0.0", Description: "fixture plugin"},
			skills:   []mockSkill{{name: "test-skill", content: "# Test Skill"}},
			agents:   []mockAgent{{name: "test-agent", content: "# Test Agent"}},
		},
	})

	runLocalGit(t, dir, "init")
	runLocalGit(t, dir, "config", "core.hooksPath", "/dev/null")
	commitLocalRepository(t, dir, "initial fixture")

	return dir
}

func addFixtureSkill(t *testing.T, repoDir, name string) {
	t.Helper()

	dir := filepath.Join(repoDir, fixturePluginName, config.SkillsDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.SkillFileName), []byte("# "+name), 0o644))
}

func commitLocalRepository(t *testing.T, dir, message string) {
	t.Helper()

	runLocalGit(t, dir, "add", "-A")
	runLocalGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", message)
}

func runLocalGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, output)
}

package loader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// A dead remote must cost one resolver attempt per window: without a negative entry
// every Resolve re-pays the git clone timeout under the per-URL lock.
func TestMarketplaceCache_ResolverErrorIsNegativelyCached(t *testing.T) {
	var callCount atomic.Int32

	resolver := func(_ context.Context, _ string) (string, error) {
		callCount.Add(1)
		return "", assert.AnError
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)
	entry := config.MarketplaceEntry{URL: "github.com/owner/repo", Plugins: []string{"plugin-a"}}

	_, _, err := cache.Resolve(context.Background(), entry)
	require.Error(t, err)
	require.ErrorIs(t, err, assert.AnError)

	_, _, err = cache.Resolve(context.Background(), entry)
	require.Error(t, err, "the failure must still be reported to the caller")
	require.ErrorIs(t, err, assert.AnError)

	assert.Equal(t, int32(1), callCount.Load(), "a failed resolve must be attempted once per window")
}

// A negative entry expires on its own shorter clock so a recovered remote is
// picked up long before the success TTL would allow.
func TestMarketplaceCache_NegativeEntryExpiresBeforeSuccessTTL(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")

	var callCount atomic.Int32

	resolver := func(_ context.Context, _ string) (string, error) {
		if callCount.Add(1) == 1 {
			return "", assert.AnError
		}

		return repoDir, nil
	}

	now := time.Now()
	cache := newMarketplaceCache(resolver, 30*time.Minute)
	impl, ok := cache.(*marketplaceCache)
	require.True(t, ok)
	impl.now = func() time.Time { return now }

	entry := config.MarketplaceEntry{URL: "github.com/owner/repo", Plugins: []string{"plugin-a"}}

	_, _, err := cache.Resolve(context.Background(), entry)
	require.Error(t, err)

	impl.now = func() time.Time { return now.Add(failedMarketplaceTTL + time.Second) }

	skills, _, err := cache.Resolve(context.Background(), entry)
	require.NoError(t, err)
	assert.Len(t, skills, 1)
	assert.Equal(t, int32(2), callCount.Load())
}

// Two config entries for the same URL with disjoint plugin lists: the loaded set
// must follow the config, not which entry warmed the cache first.
func TestMarketplaceCache_SameURLDisjointPluginLists(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")
	writeCachePlugin(t, repoDir, "plugin-b")

	var callCount atomic.Int32

	resolver := func(_ context.Context, _ string) (string, error) {
		callCount.Add(1)
		return repoDir, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)

	skillsA, agentsA, err := cache.Resolve(context.Background(), config.MarketplaceEntry{
		URL: "github.com/owner/repo", Plugins: []string{"plugin-a"},
	})
	require.NoError(t, err)
	require.Len(t, skillsA, 1)
	assert.Equal(t, "plugin-a", skillsA[0].pluginName)
	require.Len(t, agentsA, 1)

	skillsB, agentsB, err := cache.Resolve(context.Background(), config.MarketplaceEntry{
		URL: "github.com/owner/repo", Plugins: []string{"plugin-b"},
	})
	require.NoError(t, err)
	require.Len(t, skillsB, 1, "plugin-b must be scanned even though plugin-a warmed the cache")
	assert.Equal(t, "plugin-b", skillsB[0].pluginName)
	require.Len(t, agentsB, 1)

	assert.Equal(t, int32(1), callCount.Load(), "a cached repo path must be re-scanned, not re-cloned")
}

// A broken low-priority source must be skipped with the failure reported, not
// truncate the scan and silently drop every higher-priority source.
func TestLoadSkills_BrokenSourceSkippedNotFatal(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, config.AgentsConfigDir), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tempDir, config.AgentsConfigDir, config.SkillsDirName),
		[]byte("not a directory"),
		0o644,
	))

	skillDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName, "review")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, config.SkillFileName), []byte("review body"), 0o644))

	svc := New()
	err := svc.LoadSkills(tempDir)

	require.Error(t, err, "the broken source must still be reported to the caller")
	assert.Contains(t, err.Error(), filepath.Join(config.AgentsConfigDir, config.SkillsDirName))

	skill := svc.GetSkill("review")
	require.NotNil(t, skill, "a broken low-priority source must not drop higher-priority skills")
	assert.Contains(t, skill.Content, "review body")
}

func TestLoadSubagents_BrokenSourceSkippedNotFatal(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, config.ProjectCoagentDir), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tempDir, config.ProjectCoagentDir, config.AgentsDirName),
		[]byte("not a directory"),
		0o644,
	))

	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "reviewer.md"), []byte("reviewer body"), 0o644))

	svc := New()
	err := svc.LoadSubagents(tempDir)

	require.Error(t, err, "the broken source must still be reported to the caller")

	agent := svc.GetSubagent("reviewer")
	require.NotNil(t, agent, "a broken low-priority source must not drop higher-priority subagents")
	assert.Contains(t, agent.Prompt, "reviewer body")
}

// writeCachePlugin adds one manifest-bearing plugin with skills/ and agents/ to repoDir.
func writeCachePlugin(t *testing.T, repoDir, pluginName string) {
	t.Helper()

	pluginDir := filepath.Join(repoDir, pluginName)

	manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
	require.NoError(t, os.MkdirAll(manifestDir, 0o755))

	data, err := json.Marshal(pluginManifest{Name: pluginName, Version: "1.0.0"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, pluginManifestFileName), data, 0o644))

	skillsDir := filepath.Join(pluginDir, config.SkillsDirName, "test-skill")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, config.SkillFileName), []byte("# skill"), 0o644))

	agentsDir := filepath.Join(pluginDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "test-agent.md"), []byte("# agent"), 0o644))
}

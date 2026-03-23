package loader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestMarketplaceCache_ResolveTwiceWithinTTL(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")
	var callCount atomic.Int32

	resolver := func(_ context.Context, url string) (string, error) {
		callCount.Add(1)
		return repoDir, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)

	entry := config.MarketplaceEntry{
		URL:     "github.com/owner/repo",
		Plugins: []string{"plugin-a"},
	}

	skills1, agents1, err := cache.Resolve(context.Background(), entry)
	require.NoError(t, err)

	skills2, agents2, err := cache.Resolve(context.Background(), entry)
	require.NoError(t, err)

	assert.Equal(t, int32(1), callCount.Load(), "resolver should be called only once")
	assert.Equal(t, skills1, skills2)
	assert.Equal(t, agents1, agents2)
	assert.Len(t, skills1, 1)
	assert.Len(t, agents1, 1)
}

func TestMarketplaceCache_ResolveAfterTTLExpiry(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")
	var callCount atomic.Int32

	resolver := func(_ context.Context, url string) (string, error) {
		callCount.Add(1)
		return repoDir, nil
	}

	now := time.Now()
	cache := newMarketplaceCache(resolver, 5*time.Minute)
	impl := cache.(*marketplaceCache)
	impl.now = func() time.Time { return now }

	entry := config.MarketplaceEntry{
		URL:     "github.com/owner/repo",
		Plugins: []string{"plugin-a"},
	}

	_, _, err := cache.Resolve(context.Background(), entry)
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load())

	// Advance time past TTL
	impl.now = func() time.Time { return now.Add(6 * time.Minute) }

	_, _, err = cache.Resolve(context.Background(), entry)
	require.NoError(t, err)
	assert.Equal(t, int32(2), callCount.Load(), "resolver should be called again after TTL")
}

func TestMarketplaceCache_DifferentURLsIndependent(t *testing.T) {
	repoA := createCacheTestRepo(t, "plugin-a")
	repoB := createCacheTestRepo(t, "plugin-b")
	var callCount atomic.Int32

	resolver := func(_ context.Context, url string) (string, error) {
		callCount.Add(1)
		if url == "github.com/owner/repo-a" {
			return repoA, nil
		}
		return repoB, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)

	entryA := config.MarketplaceEntry{URL: "github.com/owner/repo-a", Plugins: []string{"plugin-a"}}
	entryB := config.MarketplaceEntry{URL: "github.com/owner/repo-b", Plugins: []string{"plugin-b"}}

	skillsA, _, err := cache.Resolve(context.Background(), entryA)
	require.NoError(t, err)

	skillsB, _, err := cache.Resolve(context.Background(), entryB)
	require.NoError(t, err)

	assert.Equal(t, int32(2), callCount.Load())
	assert.NotEqual(t, skillsA[0].path, skillsB[0].path)
}

func TestMarketplaceCache_ConcurrentAccess(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")
	var callCount atomic.Int32

	resolver := func(_ context.Context, url string) (string, error) {
		callCount.Add(1)
		return repoDir, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)
	entry := config.MarketplaceEntry{
		URL:     "github.com/owner/repo",
		Plugins: []string{"plugin-a"},
	}

	var wg sync.WaitGroup
	const goroutines = 20

	for range goroutines {
		wg.Go(func() {
			_, _, err := cache.Resolve(context.Background(), entry)
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	// Due to mutex, only one goroutine should trigger the resolver for the first call.
	// Subsequent goroutines find the cache populated.
	assert.Equal(t, int32(1), callCount.Load())
}

// A resolve stuck on URL A (hung remote) must not block a resolve of URL B: the
// slow git op runs under a per-URL lock, not the shared mu. Pre-fix, mu was held
// across the resolver and B would wedge behind A.
func TestMarketplaceCache_BlockedURLDoesNotBlockOthers(t *testing.T) {
	repoB := createCacheTestRepo(t, "plugin-b")

	enteredA := make(chan struct{})
	releaseA := make(chan struct{})

	t.Cleanup(func() { close(releaseA) })

	resolver := func(_ context.Context, url string) (string, error) {
		if url == "github.com/owner/repo-a" {
			close(enteredA)
			<-releaseA

			return "", assert.AnError
		}

		return repoB, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)
	entryA := config.MarketplaceEntry{URL: "github.com/owner/repo-a", Plugins: []string{"plugin-a"}}
	entryB := config.MarketplaceEntry{URL: "github.com/owner/repo-b", Plugins: []string{"plugin-b"}}

	go func() { _, _, _ = cache.Resolve(context.Background(), entryA) }()
	<-enteredA // A now holds its per-URL lock, stuck in the resolver

	doneB := make(chan error, 1)
	go func() {
		_, _, err := cache.Resolve(context.Background(), entryB)
		doneB <- err
	}()

	select {
	case err := <-doneB:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("resolve of URL B blocked behind a stuck resolve of URL A")
	}
}

func TestMarketplaceCache_ResolverError(t *testing.T) {
	resolver := func(_ context.Context, url string) (string, error) {
		return "", assert.AnError
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)
	entry := config.MarketplaceEntry{
		URL:     "github.com/owner/repo",
		Plugins: []string{"plugin-a"},
	}

	_, _, err := cache.Resolve(context.Background(), entry)
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

// A second config entry naming a plugin the repo has not scanned yet must not
// put working plugins at the mercy of the remote: the fresh entry is reused, so
// a resolver that has started failing is never asked and never cached as a
// failure over it.
func TestMarketplaceCache_FreshSuccessSurvivesALaterResolveFailure(t *testing.T) {
	repoDir := createCacheTestRepo(t, "plugin-a")

	var failing atomic.Bool

	resolver := func(context.Context, string) (string, error) {
		if failing.Load() {
			return "", assert.AnError
		}

		return repoDir, nil
	}

	cache := newMarketplaceCache(resolver, 10*time.Minute)
	first := config.MarketplaceEntry{URL: "github.com/owner/repo", Plugins: []string{"plugin-a"}}

	skills, _, err := cache.Resolve(context.Background(), first)
	require.NoError(t, err)
	require.Len(t, skills, 1)

	failing.Store(true)

	second := config.MarketplaceEntry{URL: "github.com/owner/repo", Plugins: []string{"plugin-a", "plugin-b"}}
	_, _, err = cache.Resolve(context.Background(), second)
	require.NoError(t, err, "an unscanned plugin name is not a reason to re-resolve the repo")

	skills, _, err = cache.Resolve(context.Background(), first)
	require.NoError(t, err, "the plugins that were working must keep working")
	assert.Len(t, skills, 1)
}

func TestMarketplaceCache_PluginWithCommandsDir(t *testing.T) {
	repoDir := t.TempDir()
	pluginDir := filepath.Join(repoDir, "plugin-cmd")

	manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
	require.NoError(t, os.MkdirAll(manifestDir, 0o755))
	manifest := pluginManifest{Name: "plugin-cmd", Version: "1.0.0"}
	data, _ := json.Marshal(manifest)
	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, pluginManifestFileName), data, 0o644))

	commandsDir := filepath.Join(pluginDir, config.CommandsDirName)
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(commandsDir, "cmd.md"), []byte("# cmd"), 0o644))

	cache := newMarketplaceCache(func(_ context.Context, url string) (string, error) {
		return repoDir, nil
	}, 10*time.Minute)

	entry := config.MarketplaceEntry{URL: "github.com/o/r", Plugins: []string{"plugin-cmd"}}
	_, agents, err := cache.Resolve(context.Background(), entry)
	require.NoError(t, err)
	assert.Len(t, agents, 1)
	assert.Equal(t, commandsDir, agents[0].path)
}

// createCacheTestRepo builds a temp repo with one plugin containing skills/ and agents/ dirs.
func createCacheTestRepo(t *testing.T, pluginName string) string {
	t.Helper()

	repoDir := t.TempDir()
	writeCachePlugin(t, repoDir, pluginName)

	return repoDir
}

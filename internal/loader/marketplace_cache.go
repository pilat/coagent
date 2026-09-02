package loader

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/logger"
)

const (
	defaultMarketplaceTTL = 30 * time.Minute

	// A failed resolve is remembered on a much shorter clock than a success: long
	// enough that a dead remote costs one git timeout per window, short enough to recover.
	failedMarketplaceTTL = 1 * time.Minute
)

// MarketplaceCache resolves and caches marketplace plugin sources.
type MarketplaceCache interface {
	Resolve(ctx context.Context, entry config.MarketplaceEntry) ([]sourceInfo, []sourceInfo, error)
}

var _ MarketplaceCache = (*marketplaceCache)(nil)

// marketplaceCache is a thread-safe cache for marketplace resolution and plugin processing.
// It avoids redundant git pulls and directory scans within the configured TTL.
type marketplaceCache struct {
	mu       sync.Mutex
	entries  map[string]*marketplaceCacheEntry
	keyLocks sync.Map // map[url]*sync.Mutex — per-URL, so same-URL resolves dedupe without holding mu
	ttl      time.Duration
	resolver RepositoryResolver
	now      func() time.Time // injectable clock for testing
}

// NewMarketplaceCache creates a cache backed by git with a default TTL.
func NewMarketplaceCache(gitClient git.Client) MarketplaceCache {
	return newMarketplaceCache(NewRepoResolver(gitClient), defaultMarketplaceTTL)
}

func NewRepoResolver(gitClient git.Client) RepositoryResolver {
	return func(ctx context.Context, url string) (string, error) {
		owner, repo, err := parseGitHubURL(url)
		if err != nil {
			return "", err
		}

		cacheDir, err := cacheDirForMarketplace(owner, repo)
		if err != nil {
			return "", err
		}

		if gitClient.IsCloned(ctx, cacheDir) {
			if err := gitClient.Pull(ctx, cacheDir); err != nil {
				logger.Named("loader").Warn("marketplace_pull_failed", zap.String("url", url), zap.Error(err))

				recoverMarketplace(ctx, gitClient, "https://"+url, cacheDir)
			}

			return cacheDir, nil
		}

		// A leftover dir that rev-parse rejects (half-removed, corrupted) would
		// make Clone fail with ErrDestinationExists forever; replace it.
		if _, err := os.Stat(cacheDir); err == nil {
			logger.Named("loader").Debug("marketplace_stale_dir_removed", zap.String("path", cacheDir))

			if err := os.RemoveAll(cacheDir); err != nil {
				return "", fmt.Errorf("removing unusable cache dir: %w", err)
			}
		}

		repoURL := "https://" + url
		if err := gitClient.Clone(ctx, repoURL, cacheDir); err != nil {
			return "", fmt.Errorf("cloning repository: %w", err)
		}

		return cacheDir, nil
	}
}

// newMarketplaceCache is the internal constructor for testing.
func newMarketplaceCache(resolver RepositoryResolver, ttl time.Duration) MarketplaceCache {
	return &marketplaceCache{
		entries:  make(map[string]*marketplaceCacheEntry),
		ttl:      ttl,
		resolver: resolver,
		now:      time.Now,
	}
}

// Resolve returns cached skill and agent sources for the given marketplace entry.
// If the cache entry is missing, expired, or was scanned for a different plugin set,
// it resolves and (re)scans plugins. A failed resolve is remembered too, on its own
// shorter TTL, so a dead marketplace costs one attempt per window instead of one per
// session create.
//
// The shared mu is held only for the entries lookup/store; the slow resolve (a git
// clone/pull that can block on a hung remote) runs under a per-URL lock instead, so
// one stuck URL can't wedge every other session's Resolve.
func (c *marketplaceCache) Resolve(
	ctx context.Context,
	entry config.MarketplaceEntry,
) ([]sourceInfo, []sourceInfo, error) {
	if res, ok := c.lookup(entry); ok {
		return res.skills, res.agents, res.err
	}

	unlock := c.lockURL(entry.URL)
	defer unlock()

	// Re-check: another goroutine may have resolved this URL while we waited.
	if res, ok := c.lookup(entry); ok {
		return res.skills, res.agents, res.err
	}

	cached, err := c.scan(ctx, entry)
	if err != nil {
		c.store(entry.URL, &marketplaceCacheEntry{err: err, lastPull: c.now()})
		return nil, nil, err
	}

	c.store(entry.URL, cached)

	skills, agents := filterByPlugins(cached, entry.Plugins)

	return skills, agents, nil
}

// scan produces the entry to store, reusing a still-fresh repo path so a second
// config entry for the same URL only scans its missing plugins instead of re-cloning.
func (c *marketplaceCache) scan(
	ctx context.Context,
	entry config.MarketplaceEntry,
) (*marketplaceCacheEntry, error) {
	cached, ok := c.reusable(entry.URL)
	if !ok {
		repoPath, err := c.resolver(ctx, entry.URL)
		if err != nil {
			return nil, fmt.Errorf("resolving repository %s: %w", entry.URL, err)
		}

		cached = &marketplaceCacheEntry{
			repoPath: repoPath,
			scanned:  make(map[string]struct{}, len(entry.Plugins)),
			lastPull: c.now(),
		}
	}

	for _, pluginName := range entry.Plugins {
		if _, done := cached.scanned[pluginName]; done {
			continue
		}

		cached.scanned[pluginName] = struct{}{}

		skills, agents, pluginErr := processPlugin(cached.repoPath, pluginName)
		if pluginErr != nil {
			logger.Named("loader").Warn("processing_plugin_failed",
				zap.String("url", entry.URL), zap.String("plugin", pluginName), zap.Error(pluginErr))

			continue
		}

		cached.skills = append(cached.skills, skills...)
		cached.agents = append(cached.agents, agents...)
	}

	return cached, nil
}

// lookup returns a fresh entry's answer under mu, or ok=false when work is needed.
func (c *marketplaceCache) lookup(entry config.MarketplaceEntry) (cacheResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.entries[entry.URL]
	if !ok || c.now().Sub(cached.lastPull) >= c.ttlFor(cached) {
		return cacheResult{}, false
	}

	if cached.err != nil {
		return cacheResult{err: cached.err}, true
	}

	if !cached.covers(entry.Plugins) {
		return cacheResult{}, false
	}

	skills, agents := filterByPlugins(cached, entry.Plugins)

	return cacheResult{skills: skills, agents: agents}, true
}

// reusable returns a mutable copy of a fresh successful entry for the URL.
func (c *marketplaceCache) reusable(url string) (*marketplaceCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.entries[url]
	if !ok || cached.err != nil || c.now().Sub(cached.lastPull) >= c.ttl {
		return nil, false
	}

	return cached.clone(), true
}

// ttlFor keeps the negative TTL from outliving a short success TTL in tests.
func (c *marketplaceCache) ttlFor(cached *marketplaceCacheEntry) time.Duration {
	if cached.err != nil {
		return min(c.ttl, failedMarketplaceTTL)
	}

	return c.ttl
}

func (c *marketplaceCache) store(url string, cached *marketplaceCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[url] = cached
}

// lockURL serializes resolves of the same URL without holding mu across the git op.
func (c *marketplaceCache) lockURL(url string) func() {
	actual, _ := c.keyLocks.LoadOrStore(url, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	mu.Lock()

	return mu.Unlock
}

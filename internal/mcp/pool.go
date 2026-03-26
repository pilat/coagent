package mcp

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/shellenv"
)

const defaultTTL = 30 * time.Minute

const reaperInterval = 1 * time.Minute

// failedTTL is the retry cooldown for a failed server start — short so a
// fingerprint change (fixed toolchain) or its expiry retries promptly.
const failedTTL = 2 * time.Minute

// Pool manages a shared pool of MCP clients with TTL-based lifecycle.
type Pool interface {
	// Acquire returns MCP clients for the given server configs.
	// Map keys are server names (e.g., "tavily"), used for MCPTool naming.
	// Internally deduplicates by ServerConfig.Hash().
	// Returns clients map and the config hashes (for Release).
	Acquire(
		ctx context.Context,
		configs map[string]ServerConfig,
	) (clients map[string]*Client, hashes []string, err error)

	// Release decrements refcount for the given config hashes.
	Release(hashes []string)

	// Evict retires a server's entries: idle closes now, in-use closes on its last
	// Release. Never mid-call, and never left to idle out the TTL.
	Evict(serverName string)

	// Stop shuts down all MCP clients and the reaper goroutine.
	Stop()
}

var _ Pool = (*pool)(nil)

type clientFactory func(ctx context.Context, name string, cfg ServerConfig) (*Client, error)

type poolEntry struct {
	client   *Client
	name     string // human-readable server name (from first Acquire)
	refcount int
	evicted  bool // registry row removed/disabled; close on last Release
	lastUsed time.Time
}

// failedEntry records a failed start for the retry cooldown: when, and the
// workdir env fingerprint then — a fingerprint change invalidates the cooldown.
type failedEntry struct {
	at time.Time
	fp string
}

type pool struct {
	mu            sync.Mutex
	entries       map[string]*poolEntry       // keyed by config hash
	failed        map[string]failedEntry      // hash → last start failure, for retry cooldown
	fingerprintFn func(workDir string) string // env fingerprint source; nil → cooldown is TTL-only
	ttl           time.Duration
	factory       clientFactory
	done          chan struct{}
	reaperOnce    sync.Once
	stopped       bool
}

// NewPool creates a new MCP connection pool with TTL-based lifecycle. The reaper
// goroutine starts immediately; caller owns Stop. provider (may be nil) is folded
// into the factory so every pooled server spawn routes through workDir activation.
func NewPool(provider shellenv.Provider) Pool {
	var fpFn func(string) string
	if provider != nil {
		fpFn = provider.Fingerprint
	}

	return newPoolFP(defaultTTL, fpFn, func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		return NewClient(ctx, name, cfg, provider)
	})
}

func newPool(factory clientFactory) Pool {
	return newPoolFP(5*time.Minute, nil, factory)
}

func newPoolFP(ttl time.Duration, fpFn func(string) string, factory clientFactory) *pool {
	p := &pool{
		entries:       make(map[string]*poolEntry),
		failed:        make(map[string]failedEntry),
		fingerprintFn: fpFn,
		ttl:           ttl,
		factory:       factory,
		done:          make(chan struct{}),
	}
	p.startReaper()

	return p
}

func (p *pool) Acquire(ctx context.Context, configs map[string]ServerConfig) (map[string]*Client, []string, error) {
	// Fingerprint stat-walks the workdir; compute it before the lock so it never
	// runs under pool.mu, which gates every session's acquire/release.
	fps := make(map[string]string, len(configs))
	for _, cfg := range configs {
		if _, ok := fps[cfg.WorkDir]; !ok {
			fps[cfg.WorkDir] = p.fpOf(cfg.WorkDir)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil, nil, errors.New("pool is stopped")
	}

	result := make(map[string]*Client, len(configs))
	acquired := make(map[string]struct{}) // hashes we incremented refcount for

	for name, cfg := range configs {
		hash := cfg.Hash()

		if entry, ok := p.entries[hash]; ok {
			p.joinEntry(entry, hash, name, acquired, result)
			continue
		}

		if e, ok := p.failed[hash]; ok && time.Since(e.at) < failedTTL && e.fp == fps[cfg.WorkDir] {
			continue // cooldown, and the env that broke it is unchanged — don't respawn
		}

		// Must unlock while creating client (may block on subprocess start).
		p.mu.Unlock()
		client, err := p.factory(ctx, name, cfg)
		p.mu.Lock()

		if p.stopped {
			if client != nil {
				_ = client.Close()
			}

			p.rollbackLocked(acquired)

			return nil, nil, errors.New("pool stopped during acquire")
		}

		if err != nil {
			// A broken server must not fail the whole acquire — skip it (session
			// still starts) and record the failure for the retry cooldown.
			p.failed[hash] = failedEntry{at: time.Now(), fp: fps[cfg.WorkDir]}

			logger.Ctx(ctx).Named("mcp.pool").Warn(
				"mcp_server_start_failed",
				zap.String("name", name),
				zap.String("workdir", cfg.WorkDir),
				zap.Error(err),
			)

			continue
		}

		delete(p.failed, hash) // recovered — clear any prior failure cooldown

		// Another goroutine may have created the same hash while we were unlocked.
		if entry, ok := p.entries[hash]; ok {
			_ = client.Close()

			p.joinEntry(entry, hash, name, acquired, result)

			continue
		}

		p.entries[hash] = &poolEntry{
			client:   client,
			name:     name,
			refcount: 1,
		}
		acquired[hash] = struct{}{}
		result[name] = client
	}

	hashes := make([]string, 0, len(acquired))
	for h := range acquired {
		hashes = append(hashes, h)
	}

	return result, hashes, nil
}

func (p *pool) Release(hashes []string) {
	var closing []*Client

	p.mu.Lock()

	for _, hash := range hashes {
		entry, ok := p.entries[hash]
		if !ok {
			continue
		}

		entry.refcount--
		if entry.refcount > 0 {
			continue
		}

		entry.refcount = 0
		entry.lastUsed = time.Now()

		if entry.evicted {
			delete(p.entries, hash)

			closing = append(closing, entry.client)
		}
	}

	p.mu.Unlock()

	closeClients(closing, "released")
}

// Evict drops a server by name. Closing happens outside p.mu, which gates every
// session's acquire and release.
func (p *pool) Evict(serverName string) {
	p.mu.Lock()

	var closing []*Client

	for hash, entry := range p.entries {
		if entry.name != serverName {
			continue
		}

		if entry.refcount > 0 {
			// An in-flight run keeps the tools it already registered; the entry dies
			// with the last release instead of under an active call.
			entry.evicted = true

			continue
		}

		delete(p.entries, hash)

		closing = append(closing, entry.client)
	}

	p.mu.Unlock()

	closeClients(closing, serverName)
}

func closeClients(clients []*Client, serverName string) {
	if len(clients) == 0 {
		return
	}

	log := logger.Named("mcp.pool")

	for _, c := range clients {
		if err := c.Close(); err != nil {
			log.Warn("mcp_evict_close_failed", zap.String("name", serverName), zap.Error(err))
		}
	}
}

func (p *pool) Stop() {
	log := logger.Named("mcp.pool")

	p.mu.Lock()

	if p.stopped {
		p.mu.Unlock()
		return
	}

	p.stopped = true

	close(p.done)

	// Close clients outside p.mu — Close kills a subprocess (bounded-but-nonzero),
	// and p.mu gates every session's Acquire/Release, which N sequential closes would
	// serialize for the sum.
	entries := p.entries
	p.entries = make(map[string]*poolEntry)

	p.mu.Unlock()

	for _, entry := range entries {
		log.Info("closing_client", zap.String("name", entry.name))
		_ = entry.client.Close()
	}
}

func (p *pool) startReaper() {
	p.reaperOnce.Do(func() {
		go p.reaper()
	})
}

func (p *pool) reaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.reap()
		}
	}
}

func (p *pool) reap() {
	log := logger.Named("mcp.pool")

	p.mu.Lock()

	now := time.Now()

	var toClose []*poolEntry

	for hash, entry := range p.entries {
		if entry.refcount == 0 && now.Sub(entry.lastUsed) > p.ttl {
			toClose = append(toClose, entry)

			delete(p.entries, hash)
		}
	}

	for hash, e := range p.failed {
		if now.Sub(e.at) >= failedTTL {
			delete(p.failed, hash)
		}
	}

	p.mu.Unlock()

	// Close outside p.mu so a bounded-but-nonzero Close can't serialize every
	// session's Acquire/Release behind the reaper.
	for _, entry := range toClose {
		log.Info("reaping_idle_client", zap.String("name", entry.name))
		_ = entry.client.Close()
	}
}

// fpOf returns the env fingerprint for workDir, or "" when no fingerprint source
// is wired (tests, or no provider) — leaving the cooldown TTL-only.
func (p *pool) fpOf(workDir string) string {
	if p.fingerprintFn == nil {
		return ""
	}

	return p.fingerprintFn(workDir)
}

// joinEntry attaches an already-pooled entry to the in-progress acquire,
// bumping its refcount at most once per hash.
func (p *pool) joinEntry(
	entry *poolEntry,
	hash, name string,
	acquired map[string]struct{},
	result map[string]*Client,
) {
	if _, seen := acquired[hash]; !seen {
		entry.refcount++
		acquired[hash] = struct{}{}
	}

	result[name] = entry.client
}

func (p *pool) rollbackLocked(acquired map[string]struct{}) {
	for hash := range acquired {
		if entry, ok := p.entries[hash]; ok {
			entry.refcount--
			if entry.refcount <= 0 {
				entry.refcount = 0
				entry.lastUsed = time.Now()
			}
		}
	}
}

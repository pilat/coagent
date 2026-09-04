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

// defaultCatalogTTL bounds cached tool metadata idle time — deliberately much
// longer than the live-client TTL: a catalog is data, not a subprocess.
const defaultCatalogTTL = 15 * 24 * time.Hour

const reaperInterval = 1 * time.Minute

// failedTTL is the retry cooldown for a failed server start — short so a
// fingerprint change (fixed toolchain) or its expiry retries promptly.
const failedTTL = 2 * time.Minute

// Pool manages a shared pool of MCP clients plus the tool catalogs their
// discovery produced, with separate TTL lifecycles.
type Pool interface {
	// Acquire returns one activation's snapshot of MCP access for the given
	// server configs. Map keys are server names (e.g., "tavily"), used for MCP
	// tool naming. Configs deduplicate internally by ServerConfig.Hash().
	// Each name maps either to a live client (live cache hit or cold
	// synchronous discovery) or to a cached Catalog (reaped process whose
	// discovery survives). The snapshot is stable for the activation's lifetime.
	Acquire(
		ctx context.Context,
		configs map[string]ServerConfig,
	) (*Snapshot, error)

	// Release decrements refcount for the given config hashes.
	Release(hashes []string)

	// ClientFor returns a live client for one server config, starting it if
	// needed, and holds one reference the caller releases via Release. It
	// deliberately neither reads nor writes catalogs: a lazy reconnect must
	// not replace metadata an activation already serves.
	ClientFor(ctx context.Context, name string, cfg ServerConfig) (*Client, error)

	// Invalidate drops cached catalogs and retires live entries for every
	// config hash ever acquired under serverName — across workdirs and aliases.
	// Idle entries close now; in-use entries close on their last Release.
	Invalidate(serverName string)

	// Stop shuts down all MCP clients, clears catalogs, and stops the reaper.
	Stop()
}

var _ Pool = (*pool)(nil)

// Snapshot is one activation's stable view of resolved MCP servers. Tools
// registered from it never change during the activation, even when a lazy
// ClientFor reconnect discovers different tool metadata.
type Snapshot struct {
	// Clients maps server name → live client (live cache hit or cold discovery).
	Clients map[string]*Client

	// Catalogs maps server name → cached descriptors (catalog hit, no process).
	Catalogs map[string]*Catalog

	// Hashes lists the config hashes holding one pool reference each; pass
	// them to Release when the activation closes.
	Hashes []string
}

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

// startToken marks one in-flight factory run. Invalidate sets kill so the
// finished start is discarded instead of gaining a pool entry.
type startToken struct {
	kill bool
}

type pool struct {
	mu            sync.Mutex
	entries       map[string]*poolEntry          // keyed by config hash
	catalogs      map[string]*Catalog            // config hash → cached tool metadata
	names         map[string]map[string]struct{} // server name → hashes acquired under it
	failed        map[string]failedEntry         // hash → last start failure, for retry cooldown
	inflight      map[string]map[*startToken]struct{}
	fingerprintFn func(workDir string) string // env fingerprint source; nil → cooldown is TTL-only
	ttl           time.Duration               // live-client idle TTL
	catalogTTL    time.Duration               // catalog idle TTL
	factory       clientFactory
	done          chan struct{}
	reaperOnce    sync.Once
	stopped       bool
}

// NewPool creates a new MCP connection pool with TTL-based lifecycle. The reaper
// goroutine starts immediately; caller owns Stop. provider (may be nil) is folded
// into the factory so every pooled server spawn routes through workDir activation.
func NewPool(provider shellenv.Provider) Pool {
	return NewPoolWithIdleTTL(provider, defaultTTL)
}

// NewPoolWithIdleTTL builds a pool with a non-default live-client idle TTL. It
// exists so scenario tests can exercise idle reaping against real subprocesses;
// production wiring uses NewPool.
func NewPoolWithIdleTTL(provider shellenv.Provider, ttl time.Duration) Pool {
	var fpFn func(string) string
	if provider != nil {
		fpFn = provider.Fingerprint
	}

	return newPoolFP(ttl, fpFn, func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		return NewClient(ctx, name, cfg, provider)
	})
}

func newPool(factory clientFactory) Pool {
	return newPoolFP(5*time.Minute, nil, factory)
}

func newPoolFP(ttl time.Duration, fpFn func(string) string, factory clientFactory) *pool {
	p := &pool{
		entries:       make(map[string]*poolEntry),
		catalogs:      make(map[string]*Catalog),
		names:         make(map[string]map[string]struct{}),
		failed:        make(map[string]failedEntry),
		inflight:      make(map[string]map[*startToken]struct{}),
		fingerprintFn: fpFn,
		ttl:           ttl,
		catalogTTL:    defaultCatalogTTL,
		factory:       factory,
		done:          make(chan struct{}),
	}
	p.startReaper()

	return p
}

func (p *pool) Acquire(ctx context.Context, configs map[string]ServerConfig) (*Snapshot, error) {
	// Fingerprint stat-walks the workdir; compute it before the lock so it never
	// runs under pool.mu, which gates every session's acquire/release.
	fps := make(map[string]string, len(configs))
	for _, cfg := range configs {
		if _, ok := fps[cfg.WorkDir]; !ok {
			fps[cfg.WorkDir] = p.fpOf(cfg.WorkDir)
		}
	}

	snap := &Snapshot{
		Clients:  make(map[string]*Client, len(configs)),
		Catalogs: make(map[string]*Catalog, len(configs)),
	}
	acquired := make(map[string]struct{}) // hashes we incremented refcount for
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil, errPoolStopped
	}

	for name, cfg := range configs {
		hash := cfg.Hash()

		p.trackNameLocked(name, hash)

		if entry, ok := p.entries[hash]; ok {
			p.joinEntry(entry, hash, name, acquired, snap)

			if cat, ok := p.catalogs[hash]; ok {
				cat.touch(now)
			} else if !entry.evicted {
				// An evicted entry is dying — re-planting its catalog would
				// resurrect metadata the invalidation just dropped.
				p.catalogs[hash] = newCatalog(name, entry.client, now)
			}

			continue
		}

		// Catalog hit without a live client: the registry builds from cached
		// descriptors and the process starts only on the first tool call.
		if cat, ok := p.catalogs[hash]; ok {
			cat.touch(now)
			snap.Catalogs[name] = cat

			continue
		}

		if e, ok := p.failed[hash]; ok && now.Sub(e.at) < failedTTL && e.fp == fps[cfg.WorkDir] {
			continue // cooldown, and the env that broke it is unchanged — don't respawn
		}

		client, err := p.startOrJoinClient(ctx, name, cfg, fps[cfg.WorkDir])
		if err != nil {
			if errors.Is(err, errPoolStopped) {
				p.rollbackLocked(acquired)

				return nil, errPoolStopped
			}

			continue // a broken server must not fail the whole acquire
		}

		acquired[hash] = struct{}{}
		p.recordCatalogLocked(name, client, hash, now)
		snap.Clients[name] = client
	}

	snap.Hashes = make([]string, 0, len(acquired))
	for h := range acquired {
		snap.Hashes = append(snap.Hashes, h)
	}

	return snap, nil
}

// ClientFor returns a live client for one server config, starting it if needed.
// Catalogs are deliberately neither consulted nor written: a lazy reconnect
// must not replace the metadata an activation already serves or that a registry
// mutation invalidated.
func (p *pool) ClientFor(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	fp := p.fpOf(cfg.WorkDir)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil, errPoolStopped
	}

	hash := cfg.Hash()

	p.trackNameLocked(name, hash)

	if entry, ok := p.entries[hash]; ok {
		entry.refcount++

		return entry.client, nil
	}

	if e, ok := p.failed[hash]; ok && time.Since(e.at) < failedTTL && e.fp == fp {
		return nil, errors.New("mcp server start recently failed and its retry cooldown is active")
	}

	return p.startOrJoinClient(ctx, name, cfg, fp)
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

// Invalidate drops a server's cached catalogs and retires its live entries by
// name. Closing happens outside p.mu, which gates every session's acquire and
// release. Starts still in flight for those hashes are discarded: when their
// factory returns, the fresh subprocess is closed instead of being pooled.
func (p *pool) Invalidate(serverName string) {
	p.mu.Lock()

	var closing []*Client

	for hash := range p.names[serverName] {
		delete(p.catalogs, hash)

		for tok := range p.inflight[hash] {
			tok.kill = true
		}

		entry, ok := p.entries[hash]
		if !ok {
			continue
		}

		if entry.refcount > 0 {
			// An in-flight run keeps the tools it already registered; the entry
			// dies with the last release instead of under an active call.
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
	p.catalogs = make(map[string]*Catalog)
	p.names = make(map[string]map[string]struct{})
	p.inflight = make(map[string]map[*startToken]struct{})

	p.mu.Unlock()

	for _, entry := range entries {
		log.Info("closing_client", zap.String("name", entry.name))
		_ = entry.client.Close()
	}
}

// startOrJoinClient returns a live client for the config holding one pool
// reference. It must be called with p.mu held and returns with it held; the
// factory runs with the lock released. Concurrent starters of the same hash
// race deliberately: one winner is stored, the loser's client is closed —
// no single-flight is added.
func (p *pool) startOrJoinClient(
	ctx context.Context,
	name string,
	cfg ServerConfig,
	fp string,
) (*Client, error) {
	hash := cfg.Hash()

	tok := &startToken{}
	p.addInflightLocked(hash, tok)

	// Must unlock while creating client (may block on subprocess start).
	p.mu.Unlock()
	client, err := p.factory(ctx, name, cfg)
	p.mu.Lock()

	p.removeInflightLocked(hash, tok)

	if p.stopped {
		if client != nil {
			_ = client.Close()
		}

		return nil, errPoolStopped
	}

	// The factory window can outlast a reaper tick, and the reaper prunes a
	// name association whose hash has neither entry nor catalog — re-track so
	// invalidation by name can still find the entry we may create below.
	p.trackNameLocked(name, hash)

	// Invalidate can land mid-factory. A start that raced it must not gain a
	// pool entry the invalidation already handled: discard the subprocess.
	if tok.kill {
		if client != nil {
			_ = client.Close()
		}

		return nil, errInvalidated
	}

	if err != nil {
		// A broken server must not fail the whole acquire — the caller skips it
		// (session still starts) and the failure enters the retry cooldown.
		p.failed[hash] = failedEntry{at: time.Now(), fp: fp}

		logger.Ctx(ctx).Named("mcp.pool").Warn(
			"mcp_server_start_failed",
			zap.String("name", name),
			zap.String("workdir", cfg.WorkDir),
			zap.Error(err),
		)

		return nil, err
	}

	delete(p.failed, hash) // recovered — clear any prior failure cooldown

	// Another goroutine may have created the same hash while we were unlocked.
	if entry, ok := p.entries[hash]; ok {
		_ = client.Close()

		entry.refcount++

		return entry.client, nil
	}

	p.entries[hash] = &poolEntry{
		client:   client,
		name:     name,
		refcount: 1,
	}

	return client, nil
}

func (p *pool) addInflightLocked(hash string, tok *startToken) {
	set, ok := p.inflight[hash]
	if !ok {
		set = make(map[*startToken]struct{})
		p.inflight[hash] = set
	}

	set[tok] = struct{}{}
}

func (p *pool) removeInflightLocked(hash string, tok *startToken) {
	set, ok := p.inflight[hash]
	if !ok {
		return
	}

	delete(set, tok)

	if len(set) == 0 {
		delete(p.inflight, hash)
	}
}

// recordCatalogLocked stores the tool metadata discovered by a cold start.
// Only synchronous discovery calls this; see ClientFor for why.
func (p *pool) recordCatalogLocked(name string, client *Client, hash string, now time.Time) {
	if _, ok := p.catalogs[hash]; !ok {
		p.catalogs[hash] = newCatalog(name, client, now)
	}
}

// trackNameLocked records that serverName resolved to hash. Invalidation by
// name must cover every name→hash pair ever acquired: two names with the same
// resolved configuration are aliases of one target.
func (p *pool) trackNameLocked(name, hash string) {
	hashes, ok := p.names[name]
	if !ok {
		hashes = make(map[string]struct{})
		p.names[name] = hashes
	}

	hashes[hash] = struct{}{}
}

func (p *pool) startReaper() {
	p.reaperOnce.Do(func() {
		go p.reaper(reaperTick(p.ttl, p.catalogTTL))
	})
}

// reaperTick keeps the fixed production cadence but lets short test TTLs see a
// proportionally short tick instead of waiting a full interval.
func reaperTick(ttl, catalogTTL time.Duration) time.Duration {
	tick := reaperInterval

	for _, bound := range []time.Duration{ttl / 2, catalogTTL / 2} {
		if bound > 0 && bound < tick {
			tick = bound
		}
	}

	return tick
}

func (p *pool) reaper(tick time.Duration) {
	ticker := time.NewTicker(tick)
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

	now := time.Now()

	p.mu.Lock()

	var toClose []*poolEntry

	for hash, entry := range p.entries {
		if entry.refcount == 0 && now.Sub(entry.lastUsed) > p.ttl {
			toClose = append(toClose, entry)

			delete(p.entries, hash)
		}
	}

	expiredCatalogs := p.reapCatalogsLocked(now)

	for hash, e := range p.failed {
		if now.Sub(e.at) >= failedTTL {
			delete(p.failed, hash)
		}
	}

	p.mu.Unlock()

	for _, name := range expiredCatalogs {
		log.Info("reaping_idle_catalog", zap.String("name", name))
	}

	// Close outside p.mu so a bounded-but-nonzero Close can't serialize every
	// session's Acquire/Release behind the reaper.
	for _, entry := range toClose {
		log.Info("reaping_idle_client", zap.String("name", entry.name))
		_ = entry.client.Close()
	}
}

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
	snap *Snapshot,
) {
	if _, seen := acquired[hash]; !seen {
		entry.refcount++
		acquired[hash] = struct{}{}
	}

	snap.Clients[name] = entry.client
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

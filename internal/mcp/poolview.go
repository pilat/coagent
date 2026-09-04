package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/tool"
)

// poolView implements Service over one activation's pool snapshot: the
// descriptor snapshot is immutable for the activation's lifetime, catalog-
// served tools start their server lazily on first Execute, and Stop releases
// the view's pool refs instead of closing clients.
type poolView struct {
	pool     Pool
	snap     *Snapshot
	configs  map[string]ServerConfig // resolved configs backing catalog-served names
	hashes   []string                // pool refs held: snapshot holds + lazy acquisitions
	lazyMu   sync.Mutex              // guards stopped, lazy, hashes; spans the lazy start
	lazy     map[string]*Client      // server name → lazily acquired live client
	stopped  bool
	stopOnce sync.Once
}

var _ Service = (*poolView)(nil)

func newPoolView(pool Pool, snap *Snapshot, configs map[string]ServerConfig) Service {
	return &poolView{
		pool:    pool,
		snap:    snap,
		configs: configs,
		hashes:  append([]string(nil), snap.Hashes...),
		lazy:    make(map[string]*Client),
	}
}

func (v *poolView) Start(_ context.Context, _ *Config) (*ServerStats, error) {
	// Clients are already acquired from the pool; catalogs need no start.
	stats := v.Stats()

	return &stats, nil
}

// Stop releases every pool ref the view holds, including ones acquired lazily
// after the last snapshot. lazyMu spans the blocking lazy start, so a release
// can never fall between an acquisition and its hash append.
func (v *poolView) Stop() {
	v.stopOnce.Do(func() {
		v.lazyMu.Lock()
		defer v.lazyMu.Unlock()

		v.stopped = true

		if v.pool != nil && len(v.hashes) > 0 {
			v.pool.Release(v.hashes)
		}
	})
}

func (v *poolView) RegisterTools(registry tool.Registry) int {
	count := 0

	for serverName, client := range v.snap.Clients {
		for toolName := range client.Tools() {
			registry.Register(newLiveMCPTool(serverName, toolName, client))

			count++
		}
	}

	for serverName, catalog := range v.snap.Catalogs {
		for _, meta := range catalog.Tools() {
			registry.Register(newMCPTool(serverName, meta, v.resolverFor(serverName)))

			count++
		}
	}

	return count
}

func (v *poolView) GetClient(name string) *Client {
	if c, ok := v.snap.Clients[name]; ok {
		return c
	}

	v.lazyMu.Lock()
	defer v.lazyMu.Unlock()

	return v.lazy[name]
}

func (v *poolView) Stats() ServerStats {
	// Catalog-served servers count as started: their tools are registered and
	// callable from this activation.
	total := len(v.snap.Clients) + len(v.snap.Catalogs)

	return ServerStats{
		Total:   total,
		Started: total,
	}
}

// resolverFor yields the server's live client at execution time; the reconnect
// that may result never changes the activation's descriptors.
func (v *poolView) resolverFor(serverName string) func(context.Context) (*Client, error) {
	return func(ctx context.Context) (*Client, error) {
		v.lazyMu.Lock()
		defer v.lazyMu.Unlock()

		if v.stopped {
			// The activation is already closed; nothing can release a ref
			// acquired here, so refuse instead of leaking the subprocess.
			return nil, fmt.Errorf("MCP access for %q is closed", serverName)
		}

		if c, ok := v.lazy[serverName]; ok {
			return c, nil
		}

		if c, ok := v.snap.Clients[serverName]; ok {
			return c, nil
		}

		cfg, ok := v.configs[serverName]
		if !ok {
			return nil, fmt.Errorf("no resolved MCP server named %q", serverName)
		}

		c, err := v.pool.ClientFor(ctx, serverName, cfg)
		if err != nil {
			return nil, fmt.Errorf("start MCP server %q: %w", serverName, err)
		}

		v.lazy[serverName] = c
		v.hashes = append(v.hashes, cfg.Hash())

		return c, nil
	}
}

package mcp

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/tool"
)

// poolView implements Service by wrapping pool-acquired clients.
// It does not own client lifecycle — Stop() releases pool refs
// instead of closing clients.
type poolView struct {
	pool     Pool
	clients  map[string]*Client // server name → client
	hashes   []string           // config hashes for Release()
	stopOnce sync.Once
}

var _ Service = (*poolView)(nil)

func newPoolView(pool Pool, clients map[string]*Client, hashes []string) Service {
	return &poolView{
		pool:    pool,
		clients: clients,
		hashes:  hashes,
	}
}

func (v *poolView) Start(_ context.Context, _ *Config) (*ServerStats, error) {
	// Clients are already acquired from pool — nothing to start.
	stats := v.Stats()
	return &stats, nil
}

func (v *poolView) Stop() {
	v.stopOnce.Do(func() {
		if v.pool != nil && len(v.hashes) > 0 {
			v.pool.Release(v.hashes)
		}
	})
}

func (v *poolView) RegisterTools(registry tool.Registry) int {
	count := 0

	for serverName, client := range v.clients {
		for toolName := range client.Tools() {
			registry.Register(newMCPTool(serverName, toolName, client))

			count++
		}
	}

	return count
}

func (v *poolView) GetClient(name string) *Client {
	return v.clients[name]
}

func (v *poolView) Stats() ServerStats {
	return ServerStats{
		Total:   len(v.clients),
		Started: len(v.clients),
	}
}

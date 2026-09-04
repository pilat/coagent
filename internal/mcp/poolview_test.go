package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func testSnapshot(clients map[string]*Client) *Snapshot {
	return &Snapshot{
		Clients:  clients,
		Catalogs: make(map[string]*Catalog),
	}
}

func TestPoolView_RegisterTools(t *testing.T) {
	client := &Client{
		name:   "tavily",
		client: nopMCPClient{},
		tools: map[string]mcpgo.Tool{
			"search": {Name: "search"},
			"crawl":  {Name: "crawl"},
		},
	}

	view := newPoolView(nil, testSnapshot(map[string]*Client{"tavily": client}), nil)
	registry := tool.NewRegistry()

	count := view.RegisterTools(registry)
	assert.Equal(t, 2, count)
	assert.NotNil(t, registry.Get("mcp__tavily__search"))
	assert.NotNil(t, registry.Get("mcp__tavily__crawl"))
}

func TestPoolView_GetClient(t *testing.T) {
	client := mockPoolClient("test")
	view := newPoolView(nil, testSnapshot(map[string]*Client{"test": client}), nil)

	assert.NotNil(t, view.GetClient("test"))
	assert.Nil(t, view.GetClient("missing"))
}

func TestPoolView_StopReleasesPool(t *testing.T) {
	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}
	p := newPool(factory)
	defer p.Stop()

	cfg := ServerConfig{Command: "cmd"}
	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"svc": cfg})
	require.NoError(t, err)

	view := newPoolView(p, snap, map[string]ServerConfig{"svc": cfg})

	// Before Stop: refcount should be 1
	pp := p.(*pool)
	hash := cfg.Hash()
	pp.mu.Lock()
	assert.Equal(t, 1, pp.entries[hash].refcount)
	pp.mu.Unlock()

	// Stop should release, not close
	view.Stop()

	pp.mu.Lock()
	assert.Equal(t, 0, pp.entries[hash].refcount)
	// Entry still exists (waiting for reaper)
	assert.Contains(t, pp.entries, hash)
	pp.mu.Unlock()
}

func TestPoolView_StopIsIdempotent(t *testing.T) {
	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	cfg := ServerConfig{Command: "cmd"}
	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"svc": cfg})
	require.NoError(t, err)

	view := newPoolView(p, snap, map[string]ServerConfig{"svc": cfg})

	// First Stop releases refcount to 0
	view.Stop()
	// Second Stop should be no-op, not drive refcount negative
	view.Stop()

	pp := p.(*pool)
	hash := cfg.Hash()
	pp.mu.Lock()
	assert.Equal(t, 0, pp.entries[hash].refcount)
	pp.mu.Unlock()
}

func TestPoolView_Stats(t *testing.T) {
	view := newPoolView(nil, testSnapshot(map[string]*Client{
		"a": mockPoolClient("a"),
		"b": mockPoolClient("b"),
	}), nil)

	stats := view.Stats()
	assert.Equal(t, 2, stats.Total)
	assert.Equal(t, 2, stats.Started)
	assert.Equal(t, 0, stats.Failed)
}

func TestPoolView_StartIsNoop(t *testing.T) {
	view := newPoolView(nil, testSnapshot(map[string]*Client{}), nil)
	stats, err := view.Start(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Total)
}

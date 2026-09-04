package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

// scriptedMCPClient answers tools/call with a fixed text; Close comes from
// nopMCPClient so reaping/Stop can close pooled instances.
type scriptedMCPClient struct{ nopMCPClient }

func (m *scriptedMCPClient) CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return &mcpgo.CallToolResult{
		Content: []mcpgo.Content{mcpgo.TextContent{Type: "text", Text: "pong"}},
	}, nil
}

func countingFactory(calls *int, tools map[string]mcpgo.Tool) clientFactory {
	return func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		*calls++

		return &Client{
			name:   name,
			client: &scriptedMCPClient{},
			tools:  tools,
		}, nil
	}
}

// reapNow ages the idle entry past the live TTL and runs one reaper pass: the
// same state the real reaper produces after a released stack idles out.
func reapNow(p Pool, hash string) {
	pp := p.(*pool)

	pp.mu.Lock()
	if entry, ok := pp.entries[hash]; ok {
		entry.lastUsed = time.Now().Add(-pp.ttl - time.Second)
	}
	pp.mu.Unlock()

	pp.reap()
}

func catalogTools(t *testing.T, cat *Catalog) map[string]ToolMeta {
	t.Helper()

	m := make(map[string]ToolMeta, len(cat.Tools()))
	for _, meta := range cat.Tools() {
		m[meta.Name] = meta
	}

	return m
}

// Reaping an idle client must leave its catalog present and drop every
// reference to the client itself.
func TestPool_CatalogOutlivesReapedClient(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, map[string]mcpgo.Tool{
		"ping": {Name: "ping", Description: "Answers pong."},
	}))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	require.Contains(t, snap.Clients, "srv")

	p.Release(snap.Hashes)
	reapNow(p, hash)

	pp := p.(*pool)
	pp.mu.Lock()
	_, clientReaped := pp.entries[hash]
	cat := pp.catalogs[hash]
	pp.mu.Unlock()

	assert.False(t, clientReaped, "the idle client was reaped")
	require.NotNil(t, cat, "the catalog survives the reaped client")

	metas := catalogTools(t, cat)
	require.Len(t, metas, 1)
	assert.Equal(t, "Answers pong.", metas["ping"].Description)
	assert.NotEmpty(t, metas["ping"].Schema)
}

// A catalog-hit acquire must not start a process: the activation receives
// descriptors, not clients.
func TestPool_CatalogHitSkipsSpawn(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, map[string]mcpgo.Tool{
		"ping": {Name: "ping"},
	}))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, hash)

	snap, err = p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	assert.Empty(t, snap.Clients, "a reaped client must not be replaced by acquire")
	require.Contains(t, snap.Catalogs, "srv")
	assert.Len(t, snap.Catalogs["srv"].Tools(), 1)
	assert.Equal(t, 1, calls, "acquire must not spawn on a catalog hit")
}

// A catalog unused for more than 15 days is removed by the reaper.
func TestPool_CatalogExpiresAfterFifteenDaysIdle(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, nil))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, cfg.Hash())

	pp := p.(*pool)
	pp.mu.Lock()
	pp.catalogs[cfg.Hash()].lastUsed = time.Now().Add(-defaultCatalogTTL - time.Minute)
	pp.mu.Unlock()

	pp.reap()

	pp.mu.Lock()
	_, alive := pp.catalogs[cfg.Hash()]
	pp.mu.Unlock()
	assert.False(t, alive, "a catalog idle beyond its TTL must be evicted")
}

// Catalog use refreshes its idle clock: re-acquires keep it alive.
func TestPool_CatalogUseRefreshesLastUsed(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, nil))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, hash)

	pp := p.(*pool)
	pp.mu.Lock()
	pp.catalogs[hash].lastUsed = time.Now().Add(-defaultCatalogTTL + time.Hour)
	pp.mu.Unlock()

	_, err = p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)

	pp.mu.Lock()
	lastUsed := pp.catalogs[hash].lastUsed
	pp.mu.Unlock()
	assert.Less(t, time.Since(lastUsed), time.Hour,
		"catalog use must refresh the idle clock forward")
}

// A catalog entry retains no client, transport, or context reference. The type
// structure is the guard: Catalog and ToolMeta are plain value payloads with
// no pointer fields, so nothing can keep a live object reachable.
func TestCatalog_RetainsNoClientReferences(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[Catalog](),
		reflect.TypeFor[ToolMeta](),
	} {
		for i := range typ.NumField() {
			field := typ.Field(i)
			assert.NotEqual(t, reflect.Pointer, field.Type.Kind(),
				"%s.%s must not hold a reference", typ.Name(), field.Name)
		}
	}
}

// A pool stop clears catalog metadata and closes only live clients.
func TestPool_StopClearsCatalogs(t *testing.T) {
	var calls int
	var closed atomic.Int32

	p := newPool(func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		calls++
		c := mockPoolClient(name)
		c.cancelRun = func() { closed.Add(1) }

		return c, nil
	})
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, cfg.Hash())

	p.Stop()

	pp := p.(*pool)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.Empty(t, pp.entries)
	assert.Empty(t, pp.catalogs)
	assert.Empty(t, pp.names)
	// Exactly one close: the reap closed the reaped client; Stop found no
	// entries left and must not close anything again.
	assert.Equal(t, int32(1), closed.Load())
}

// ClientFor lazily starts the missing client, deduplicates concurrent or
// sequential acquirers by refcount, and releases exactly on Release.
func TestPool_ClientForLazilyStartsAndJoins(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, nil))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	c1, err := p.ClientFor(context.Background(), "srv", cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "no live client → lazily started")

	c2, err := p.ClientFor(context.Background(), "srv", cfg)
	require.NoError(t, err)
	assert.Same(t, c1, c2)
	assert.Equal(t, 1, calls, "a live client is joined, not restarted")

	pp := p.(*pool)
	pp.mu.Lock()
	assert.Equal(t, 2, pp.entries[hash].refcount)
	pp.mu.Unlock()

	p.Release([]string{hash, hash})
	pp.mu.Lock()
	assert.Equal(t, 0, pp.entries[hash].refcount)
	pp.mu.Unlock()
}

// A lazy reconnect deliberately writes no catalog: reconnecting must not
// replace or plant metadata.
func TestPool_ClientForRecordsNoCatalog(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, nil))
	t.Cleanup(p.Stop)

	_, err := p.ClientFor(context.Background(), "srv", ServerConfig{Command: "cmd"})
	require.NoError(t, err)

	pp := p.(*pool)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.Empty(t, pp.catalogs)
}

// Invalidation by one name must cover aliases: a second server name resolving
// to the same configuration is the same target.
func TestPool_InvalidateCoversAliasNames(t *testing.T) {
	var closed atomic.Int32

	p := newPool(closeTrackingFactory(&closed))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	first, err := p.Acquire(context.Background(), map[string]ServerConfig{"primary": cfg})
	require.NoError(t, err)
	p.Release(first.Hashes)

	// The alias joins the existing entry later, under a different name.
	second, err := p.Acquire(context.Background(), map[string]ServerConfig{"alias": cfg})
	require.NoError(t, err)
	p.Release(second.Hashes)

	pp := p.(*pool)
	pp.mu.Lock()
	require.NotNil(t, pp.catalogs[hash], "precondition: catalog exists")
	pp.mu.Unlock()

	p.Invalidate("alias")

	pp.mu.Lock()
	_, catalogAlive := pp.catalogs[hash]
	_, entryAlive := pp.entries[hash]
	pp.mu.Unlock()

	assert.False(t, catalogAlive, "invalidation by an alias name must drop the catalog")
	assert.False(t, entryAlive, "invalidation by an alias name must retire the live entry")
	assert.Equal(t, int32(1), closed.Load())
}

// The reaper can tick while a start is in flight (the lock is released for the
// factory). The name association must survive that window, or invalidation by
// name goes blind for the entry the start creates.
func TestPool_InvalidateSurvivesReaperDuringInFlightStart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	p := newPool(func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		close(started)

		<-release

		return mockPoolClient(name), nil
	})
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	acqDone := make(chan struct{})
	var hashes []string

	go func() {
		defer close(acqDone)

		snap, _ := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
		hashes = snap.Hashes
	}()

	<-started
	p.(*pool).reap() // a tick lands while the factory is still running
	close(release)
	<-acqDone

	pp := p.(*pool)
	pp.mu.Lock()
	require.Contains(t, pp.entries, hash, "precondition: the start completed")
	pp.mu.Unlock()

	p.Release(hashes)
	p.Invalidate("srv")

	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.NotContains(t, pp.entries, hash,
		"invalidation must find entries created after a reaper tick during their start")
	assert.NotContains(t, pp.catalogs, hash)
}

// Invalidate landing while a lazy start's factory is still running must kill
// that start: the fresh subprocess is discarded and no entry or catalog
// appears for the invalidated server.
func TestPool_InvalidateDuringInFlightLazyStartDiscardsClient(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closed atomic.Int32

	p := newPool(func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		close(started)
		<-release

		c := mockPoolClient(name)
		c.cancelRun = func() { closed.Add(1) }

		return c, nil
	})
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	res := make(chan error, 1)
	go func() {
		_, err := p.ClientFor(context.Background(), "srv", cfg)
		res <- err
	}()

	<-started
	p.Invalidate("srv") // lands mid-factory, before any entry exists
	close(release)

	require.ErrorIs(t, <-res, errInvalidated)
	assert.Equal(t, int32(1), closed.Load(),
		"the freshly started subprocess must be discarded, not pooled")

	pp := p.(*pool)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.NotContains(t, pp.entries, hash,
		"a start that raced invalidation must not gain a pool entry")
	assert.NotContains(t, pp.catalogs, hash)
}

// The same kill works by alias: the start runs under a name the invalidation
// does not target, but the hash was previously acquired under that name.
func TestPool_InvalidateByAliasNameKillsInFlightStart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closed, calls atomic.Int32

	// Only the lazy start blocks in the factory; the priming acquire completes.
	p := newPool(func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		c := mockPoolClient(name)

		if calls.Add(1) == 2 {
			close(started)
			<-release

			c.cancelRun = func() { closed.Add(1) }
		}

		return c, nil
	})
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	// "primary" registers the hash association; its entry and catalog are
	// then removed so invalidation has no entry left to retire.
	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"primary": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, hash)

	pp := p.(*pool)
	pp.mu.Lock()
	require.NotContains(t, pp.entries, hash)
	pp.mu.Unlock()

	res := make(chan error, 1)
	go func() {
		_, err := p.ClientFor(context.Background(), "alias", cfg)
		res <- err
	}()

	<-started
	p.Invalidate("primary")
	close(release)

	require.ErrorIs(t, <-res, errInvalidated)
	assert.Equal(t, int32(1), closed.Load())
}

// Joining an in-use entry that invalidation already retired must not re-plant
// the catalog the invalidation dropped: the entry is dying, and a re-planted
// catalog would outlive it by the catalog TTL.
func TestPool_AcquireDoesNotReplantCatalogOfEvictedEntry(t *testing.T) {
	var calls int

	p := newPool(countingFactory(&calls, map[string]mcpgo.Tool{
		"ping": {Name: "ping"},
	}))
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	first, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)

	p.Invalidate("srv") // in-use: entry marked evicted, catalog dropped

	pp := p.(*pool)
	pp.mu.Lock()
	require.True(t, pp.entries[hash].evicted)
	pp.mu.Unlock()

	second, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	require.Contains(t, second.Clients, "srv")

	p.Release(first.Hashes)
	p.Release(second.Hashes)
	reapNow(p, hash)

	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.NotContains(t, pp.catalogs, hash,
		"an evicted entry must not re-plant the invalidated catalog")
}

// A failed start never plants a catalog, and its cooldown applies to the lazy
// path too.
func TestPool_ClientForRespectsFailedCooldown(t *testing.T) {
	var calls int
	factory := func(_ context.Context, _ string, _ ServerConfig) (*Client, error) {
		calls++

		return nil, fmt.Errorf("dead")
	}

	p := newPool(factory)
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}

	_, err := p.ClientFor(context.Background(), "srv", cfg)
	require.Error(t, err)
	assert.Equal(t, 1, calls)

	_, err = p.ClientFor(context.Background(), "srv", cfg)
	require.Error(t, err)
	assert.Equal(t, 1, calls, "lazy start must respect the failed-start cooldown")
}

// A catalog-served activation registers the cached direct tools, reaches its
// registry without spawning, starts the server only on first Execute, and
// releases the lazy pool reference exactly once on Stop. The reconnected
// server's changed tools/list must not alter the activation's descriptors.
func TestPoolView_CatalogServedToolExecutesLazily(t *testing.T) {
	var calls int

	// The reconnected (second) client advertises a different tool set — the
	// detector: the snapshot's descriptors must survive it.
	factory := func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		calls++

		tools := map[string]mcpgo.Tool{
			"ping": {Name: "ping", Description: "Answers pong."},
		}
		if calls > 1 {
			tools = map[string]mcpgo.Tool{
				"ping":  {Name: "ping", Description: "Answers pong, now with extras."},
				"ping2": {Name: "ping2", Description: "Second-spawn-only tool."},
			}
		}

		return &Client{
			name:   name,
			client: &scriptedMCPClient{},
			tools:  tools,
		}, nil
	}

	p := newPool(factory)
	t.Cleanup(p.Stop)

	cfg := ServerConfig{Command: "cmd"}

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	p.Release(snap.Hashes)
	reapNow(p, cfg.Hash())

	snap, err = p.Acquire(context.Background(), map[string]ServerConfig{"srv": cfg})
	require.NoError(t, err)
	require.Contains(t, snap.Catalogs, "srv", "precondition: catalog hit")
	require.Empty(t, snap.Clients)

	view := newPoolView(p, snap, map[string]ServerConfig{"srv": cfg}).(*poolView)

	registry := tool.NewRegistry()
	assert.Equal(t, 1, view.RegisterTools(registry))
	assert.Equal(t, 1, calls, "registry construction must not spawn")

	tl := registry.Get("mcp__srv__ping")
	require.NotNil(t, tl)
	descBefore := tl.Description()
	schemaBefore := tl.Parameters()

	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "pong", res.Output)

	_, err = tl.Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "sequential calls reuse the lazily acquired client")

	assert.Len(t, registry.IDs(), 1, "the reconnect must not add tools")
	assert.Equal(t, descBefore, tl.Description(), "the reconnect must not alter descriptors")
	assert.JSONEq(t, string(schemaBefore), string(tl.Parameters()),
		"the reconnect must not alter schemas")

	view.Stop()

	pp := p.(*pool)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	assert.Equal(t, 0, pp.entries[cfg.Hash()].refcount,
		"the lazy acquisition was released exactly once")
}

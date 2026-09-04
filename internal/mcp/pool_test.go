package mcp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopMCPClient is a minimal mock that satisfies client.MCPClient
// with a no-op Close. Other methods will panic if called (they shouldn't be).
type nopMCPClient struct{ mcpclient.MCPClient }

func (nopMCPClient) Close() error { return nil }

// mockPoolClient creates a minimal Client for testing purposes.
func mockPoolClient(name string) *Client {
	return &Client{
		name:   name,
		client: nopMCPClient{},
		tools:  make(map[string]mcpgo.Tool),
	}
}

func TestHash_Deterministic(t *testing.T) {
	cfg := ServerConfig{
		Command: "npx",
		Args:    []string{"-y", "tavily-mcp"},
		Env:     map[string]string{"KEY": "val", "ANOTHER": "val2"},
		WorkDir: "/tmp/test",
	}

	h1 := cfg.Hash()
	h2 := cfg.Hash()
	assert.Equal(t, h1, h2, "same config must produce same hash")
	assert.Len(t, h1, 64, "SHA-256 hex digest must be 64 chars")
}

func TestHash_DifferentConfigs(t *testing.T) {
	cfg1 := ServerConfig{Command: "cmd1", Args: []string{"a"}}
	cfg2 := ServerConfig{Command: "cmd2", Args: []string{"a"}}

	assert.NotEqual(t, cfg1.Hash(), cfg2.Hash())
}

func TestHash_IgnoresDisabledEnabled(t *testing.T) {
	cfg1 := ServerConfig{Command: "cmd", Disabled: false}
	cfg2 := ServerConfig{Command: "cmd", Disabled: true}
	cfg3 := ServerConfig{Command: "cmd", Enabled: boolPtr(false)}

	assert.Equal(t, cfg1.Hash(), cfg2.Hash())
	assert.Equal(t, cfg1.Hash(), cfg3.Hash())
}

func TestHash_ArgsOrderMatters(t *testing.T) {
	cfg1 := ServerConfig{Command: "cmd", Args: []string{"--host", "localhost"}}
	cfg2 := ServerConfig{Command: "cmd", Args: []string{"localhost", "--host"}}

	assert.NotEqual(t, cfg1.Hash(), cfg2.Hash(), "arg order is significant")
}

func TestHash_EnvOrderIndependent(t *testing.T) {
	cfg := ServerConfig{
		Command: "cmd",
		Env:     map[string]string{"Z": "1", "A": "2", "M": "3"},
	}

	hashes := make(map[string]struct{})
	for range 100 {
		hashes[cfg.Hash()] = struct{}{}
	}
	assert.Len(t, hashes, 1, "hash must be stable regardless of map iteration order")
}

func TestPool_AcquireSameConfigDifferentNames(t *testing.T) {
	cfg := ServerConfig{Command: "echo", Args: []string{"hello"}}
	var created int

	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		created++
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"alias1": cfg,
		"alias2": cfg,
	})
	require.NoError(t, err)
	assert.Len(t, snap.Clients, 2)
	assert.Len(t, snap.Hashes, 1, "same config should produce one hash")

	// Both names should resolve to the same underlying client.
	assert.Same(t, snap.Clients["alias1"], snap.Clients["alias2"])
	// Factory should have been called only once.
	assert.Equal(t, 1, created)
}

// Two concurrent starts of the same config race deliberately: both run the
// factory, the winner is pooled, the loser's subprocess is closed, and both
// callers leave holding one refcount each.
func TestPool_ConcurrentStartsDeduplicateAndCloseLoser(t *testing.T) {
	var created, closed atomic.Int32
	release := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)

	p := newPool(func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		created.Add(1)
		entered.Done()
		<-release

		c := mockPoolClient(name)
		c.cancelRun = func() { closed.Add(1) }

		return c, nil
	})
	defer p.Stop()

	cfg := ServerConfig{Command: "cmd"}
	hash := cfg.Hash()

	const starters = 2
	errs := make(chan error, starters)
	for range starters {
		go func() {
			_, err := p.ClientFor(context.Background(), "srv", cfg)
			errs <- err
		}()
	}

	// Both starters must sit inside the factory before releasing them, or the
	// late one joins the pooled entry and the race never happens.
	entered.Wait()
	close(release)

	for range starters {
		require.NoError(t, <-errs)
	}

	assert.Equal(t, int32(starters), created.Load(),
		"the pool races concurrent starts instead of single-flighting them")

	pp := p.(*pool)
	pp.mu.Lock()
	require.Len(t, pp.entries, 1)
	assert.Equal(t, starters, pp.entries[hash].refcount)
	pp.mu.Unlock()

	assert.Equal(t, int32(1), closed.Load(), "exactly the loser's subprocess is closed")

	p.Release([]string{hash, hash})
	pp.mu.Lock()
	assert.Equal(t, 0, pp.entries[hash].refcount)
	pp.mu.Unlock()
}

func TestPool_AcquireDifferentConfigs(t *testing.T) {
	cfg1 := ServerConfig{Command: "cmd1"}
	cfg2 := ServerConfig{Command: "cmd2"}
	var created int

	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		created++
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"svc1": cfg1,
		"svc2": cfg2,
	})
	require.NoError(t, err)
	assert.Len(t, snap.Clients, 2)
	assert.Len(t, snap.Hashes, 2)
	assert.NotSame(t, snap.Clients["svc1"], snap.Clients["svc2"])
	assert.Equal(t, 2, created)
}

func TestPool_ReleaseDecrementsRefcount(t *testing.T) {
	cfg := ServerConfig{Command: "cmd"}

	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"a": cfg,
		"b": cfg,
	})
	require.NoError(t, err)

	pp := p.(*pool)
	hash := cfg.Hash()

	// Same config → one hash entry, refcount=1 (deduplicated)
	pp.mu.Lock()
	assert.Equal(t, 1, pp.entries[hash].refcount)
	pp.mu.Unlock()

	p.Release(snap.Hashes)
	pp.mu.Lock()
	assert.Equal(t, 0, pp.entries[hash].refcount)
	pp.mu.Unlock()
}

func TestPool_StopClosesAll(t *testing.T) {
	cfg1 := ServerConfig{Command: "cmd1"}
	cfg2 := ServerConfig{Command: "cmd2"}

	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)

	_, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"a": cfg1,
		"b": cfg2,
	})
	require.NoError(t, err)

	p.Stop()

	pp := p.(*pool)
	pp.mu.Lock()
	assert.Empty(t, pp.entries)
	pp.mu.Unlock()
}

// Stop must actually close every pooled client (Close runs cancelRun first) and
// clear both maps — the close happens outside p.mu.
func TestPool_StopClosesClientsAndClearsMaps(t *testing.T) {
	var closed atomic.Int32

	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		c := mockPoolClient(name)
		c.cancelRun = func() { closed.Add(1) }

		return c, nil
	}

	p := newPool(factory)

	_, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"a": {Command: "cmd1"},
		"b": {Command: "cmd2"},
	})
	require.NoError(t, err)

	p.Stop()

	pp := p.(*pool)
	pp.mu.Lock()
	assert.Empty(t, pp.entries)
	assert.Empty(t, pp.failed)
	assert.Empty(t, pp.catalogs)
	pp.mu.Unlock()

	assert.Equal(t, int32(2), closed.Load(), "every pooled client must be closed by Stop")
}

// The regression target of F7: a slow Close in Stop must not hold p.mu, which
// gates every session's Acquire/Release. A blocking Close + a concurrent Release
// that must still return promptly proves the close runs outside the lock — revert
// to closing under the lock and this hangs.
func TestPool_StopClosesOutsideLock(t *testing.T) {
	block := make(chan struct{})
	closing := make(chan struct{})

	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		c := mockPoolClient(name)
		c.cancelRun = func() { // Client.Close calls cancelRun first
			close(closing)
			<-block
		}

		return c, nil
	}

	p := newPool(factory)

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"a": {Command: "cmd1"}})
	require.NoError(t, err)

	go p.Stop()

	<-closing // Stop is now blocked inside Close, having released p.mu (or, pre-fix, still holding it)

	relDone := make(chan struct{})
	go func() {
		p.Release(snap.Hashes)
		close(relDone)
	}()

	select {
	case <-relDone:
	case <-time.After(5 * time.Second):
		close(block)
		t.Fatal("Release blocked behind an in-flight Close held under p.mu")
	}

	close(block)
}

func TestPool_ReleaseUnknownHashIsNoop(t *testing.T) {
	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	// Should not panic.
	p.Release([]string{"nonexistent-hash"})
}

func TestPool_StopIsIdempotent(t *testing.T) {
	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)

	// Multiple stops should not panic.
	p.Stop()
	p.Stop()
}

func TestPool_AcquireAfterStop(t *testing.T) {
	factory := func(_ context.Context, name string, c ServerConfig) (*Client, error) {
		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	p.Stop()

	_, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"svc": {Command: "cmd"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped")
}

func TestPool_AcquireSkipsFailedServer(t *testing.T) {
	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		if name == "bad" {
			return nil, fmt.Errorf("server won't start")
		}

		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	// One server fails to start; the acquire must still succeed with the rest so
	// a single broken MCP server never blocks the session from running.
	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"good": {Command: "cmd1"},
		"bad":  {Command: "cmd2"},
	})
	require.NoError(t, err)
	assert.Contains(t, snap.Clients, "good")
	assert.NotContains(t, snap.Clients, "bad")
	assert.Len(t, snap.Hashes, 1, "only the good server holds a refcount")
}

func TestPool_AcquireAllFailedReturnsEmpty(t *testing.T) {
	factory := func(_ context.Context, _ string, _ ServerConfig) (*Client, error) {
		return nil, fmt.Errorf("nope")
	}

	p := newPool(factory)
	defer p.Stop()

	// Every server failing still yields a usable (empty) result, not an error —
	// the session runs with no MCP tools rather than refusing to start.
	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"a": {Command: "x"},
		"b": {Command: "y"},
	})
	require.NoError(t, err)
	assert.Empty(t, snap.Clients)
	assert.Empty(t, snap.Hashes)
}

func TestPool_NegativeCacheSuppressesRetry(t *testing.T) {
	var calls int
	factory := func(_ context.Context, _ string, _ ServerConfig) (*Client, error) {
		calls++
		return nil, fmt.Errorf("dead server")
	}

	p := newPool(factory)
	defer p.Stop()

	cfg := map[string]ServerConfig{"srv": {Command: "x"}}

	// First acquire hits the factory and records the failure.
	_, err := p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)

	// Second acquire within the cooldown must NOT respawn the known-dead server.
	_, err = p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "dead server retried during cooldown")

	// Age the failure past the cooldown → the next acquire retries once.
	pp := p.(*pool)
	pp.mu.Lock()
	for h := range pp.failed {
		pp.failed[h] = failedEntry{at: time.Now().Add(-failedTTL - time.Second)}
	}
	pp.mu.Unlock()

	_, err = p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "server not retried after cooldown expired")
}

func TestPool_NegativeCacheClearedOnRecovery(t *testing.T) {
	var calls int
	factory := func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("dead once")
		}

		return mockPoolClient(name), nil
	}

	p := newPool(factory)
	defer p.Stop()

	cfg := map[string]ServerConfig{"srv": {Command: "x"}}

	_, err := p.Acquire(context.Background(), cfg)
	require.NoError(t, err)

	// Age past the cooldown so the next acquire retries and succeeds.
	pp := p.(*pool)
	pp.mu.Lock()
	for h := range pp.failed {
		pp.failed[h] = failedEntry{at: time.Now().Add(-failedTTL - time.Second)}
	}
	pp.mu.Unlock()

	snap, err := p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Contains(t, snap.Clients, "srv")

	// Recovery must clear the cooldown entry so it can't linger.
	pp.mu.Lock()
	_, stillFailed := pp.failed[cfg["srv"].Hash()]
	pp.mu.Unlock()
	assert.False(t, stillFailed, "failure cooldown must clear once the server recovers")
}

func TestPool_NegativeCacheInvalidatedByFingerprintChange(t *testing.T) {
	var calls int
	factory := func(_ context.Context, _ string, _ ServerConfig) (*Client, error) {
		calls++
		return nil, fmt.Errorf("dead")
	}

	fp := "env-A"
	p := newPoolFP(5*time.Minute, func(string) string { return fp }, factory)
	defer p.Stop()

	cfg := map[string]ServerConfig{"srv": {Command: "x", WorkDir: "/w"}}

	_, err := p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)

	// Same fingerprint, within cooldown → not retried.
	_, err = p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "same env within cooldown must not retry")

	// The env fingerprint changes (toolchain fixed) → retried at once despite the cooldown.
	fp = "env-B"
	_, err = p.Acquire(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "a fingerprint change must invalidate the cooldown")
}

func closeTrackingFactory(closed *atomic.Int32) clientFactory {
	return func(_ context.Context, name string, _ ServerConfig) (*Client, error) {
		c := mockPoolClient(name)
		c.cancelRun = func() { closed.Add(1) }

		return c, nil
	}
}

// Invalidate retires an idle entry immediately and, as part of the same
// contract, drops its catalog so the next activation rediscovers from scratch.
func TestPoolInvalidateClosesAnIdleEntryImmediately(t *testing.T) {
	var closed atomic.Int32

	p := newPool(closeTrackingFactory(&closed))
	t.Cleanup(p.Stop)

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{
		"gone": {Command: "cmd1"},
		"kept": {Command: "cmd2"},
	})
	require.NoError(t, err)

	p.Release(snap.Hashes)
	require.Equal(t, int32(0), closed.Load(), "release alone leaves the entry pooled for the TTL")

	p.Invalidate("gone")

	assert.Equal(t, int32(1), closed.Load())

	pp := p.(*pool)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	require.Len(t, pp.entries, 1)
	assert.NotContains(t, pp.catalogs, ServerConfig{Command: "cmd1"}.Hash(), "invalidation drops the catalog")

	for _, entry := range pp.entries {
		assert.Equal(t, "kept", entry.name)
	}
}

// A live stack keeps the tools it already registered, so an in-use entry must not
// have its subprocess killed under an active call — it dies on the last release.
func TestPoolInvalidateDefersCloseWhileInUse(t *testing.T) {
	var closed atomic.Int32

	p := newPool(closeTrackingFactory(&closed))
	t.Cleanup(p.Stop)

	first, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": {Command: "cmd"}})
	require.NoError(t, err)

	second, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": {Command: "cmd"}})
	require.NoError(t, err)

	p.Invalidate("srv")
	assert.Equal(t, int32(0), closed.Load(), "still referenced")

	p.Release(first.Hashes)
	assert.Equal(t, int32(0), closed.Load(), "one holder left")

	p.Release(second.Hashes)
	assert.Equal(t, int32(1), closed.Load(), "last release retires the evicted entry")

	pp := p.(*pool)
	pp.mu.Lock()
	assert.Empty(t, pp.entries)
	pp.mu.Unlock()
}

func TestPoolInvalidateSpansEveryWorkdirOfAServer(t *testing.T) {
	var closed atomic.Int32

	p := newPool(closeTrackingFactory(&closed))
	t.Cleanup(p.Stop)

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": {Command: "cmd", WorkDir: "/a"}})
	require.NoError(t, err)
	p.Release(snap.Hashes)

	snap, err = p.Acquire(context.Background(), map[string]ServerConfig{"srv": {Command: "cmd", WorkDir: "/b"}})
	require.NoError(t, err)
	p.Release(snap.Hashes)

	p.Invalidate("srv")
	assert.Equal(t, int32(2), closed.Load(), "one server name can back several workdir entries")
}

func TestPoolInvalidateUnknownNameIsANoop(t *testing.T) {
	var closed atomic.Int32

	p := newPool(closeTrackingFactory(&closed))
	t.Cleanup(p.Stop)

	snap, err := p.Acquire(context.Background(), map[string]ServerConfig{"srv": {Command: "cmd"}})
	require.NoError(t, err)
	p.Release(snap.Hashes)

	p.Invalidate("never-pooled")
	assert.Equal(t, int32(0), closed.Load())
}

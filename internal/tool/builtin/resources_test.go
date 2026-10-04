package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/mcp"
)

type countedResourceMCP struct {
	mcp.Service
	stops int
}

func (m *countedResourceMCP) Stop() { m.stops++ }

func TestResourcesModelLifetime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	type state struct {
		lease           *resourceLease
		client          *countedResourceMCP
		active, retired bool
	}
	owner := NewResources()
	defer func() { require.NoError(t, owner.Close()) }()
	model := map[int64]*state{}
	closed := false
	steps := []struct {
		op string
		id int64
	}{
		{"acquire", 1},
		{"acquire", 1},
		{"release", 1},
		{"acquire", 1},
		{"acquire", 2},
		{"retire", 1},
		{"acquire", 1},
		{"release", 1},
		{"acquire", 1},
		{"release", 2},
		{"retire", 2},
		{"acquire", 2},
		{"release", 2},
		{"close", 0},
		{"acquire", 2},
		{"release", 1},
		{"close", 0},
	}
	for _, step := range steps {
		previous := model[step.id]
		switch step.op {
		case "acquire":
			lease, err := owner.acquire(StackConfig{SessionID: step.id, WorkDir: "/project"}, bashsandbox.Config{})
			if closed || previous != nil && previous.active {
				require.Error(t, err)
				continue
			}
			require.NoError(t, err)
			if previous != nil && !previous.retired {
				require.Same(t, previous.lease, lease)
				previous.active = true
			} else {
				if previous != nil {
					require.NotSame(t, previous.lease, lease)
				}
				client := &countedResourceMCP{}
				lease.mcp = client
				model[step.id] = &state{lease: lease, client: client, active: true}
			}
		case "release":
			require.NoError(t, previous.lease.release())
			previous.active = false
		case "retire":
			require.NoError(t, owner.Retire(step.id))
			previous.retired = true
		case "close":
			require.NoError(t, owner.Close())
			closed = true
			for _, entry := range model {
				entry.retired = true
			}
		}
		for _, entry := range model {
			wantStops := 0
			if entry.retired && !entry.active {
				wantStops = 1
			}
			assert.Equal(t, wantStops, entry.client.stops, "%s session %d", step.op, step.id)
		}
	}
}

func TestBuildStackResourcesReuseShellSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("SHELL", "/bin/bash")
	owner := NewResources()
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	cfg := StackConfig{SessionID: 1, WorkDir: home, Resources: owner}
	first, err := BuildStack(t.Context(), cfg)
	require.NoError(t, err)
	path := first.resources.provider.Snapshot(t.Context(), nil, home)
	require.NotEmpty(t, path)
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, first.Close())
	second, err := BuildStack(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	assert.Same(t, first.resources.provider, second.resources.provider)
	afterPath := second.resources.provider.Snapshot(t.Context(), nil, home)
	after, err := os.Stat(afterPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "resume must not recapture the activated shell")
	cfg.SessionID = 2
	child, err := BuildStack(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, child.Close()) })
	assert.NotSame(t, second.resources.provider, child.resources.provider)
}

func TestResourcePolicyChangeRetiresPriorStack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	owner := NewResources()
	defer func() { require.NoError(t, owner.Close()) }()
	cfg := StackConfig{SessionID: 1, ProjectID: 7, WorkDir: "/project"}
	first, err := owner.acquire(cfg, bashsandbox.Config{})
	require.NoError(t, err)
	client := &countedResourceMCP{}
	first.mcp = client
	require.NoError(t, first.release())
	second, err := owner.acquire(cfg, bashsandbox.Config{Enabled: true})
	require.NoError(t, err)
	assert.NotSame(t, first.provider, second.provider)
	assert.Equal(t, 1, client.stops)
	require.NoError(t, second.release())
}

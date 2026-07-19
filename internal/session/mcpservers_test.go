package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
)

var _ mcpstore.Store = (*fakeMCPStore)(nil)

type fakeMCPStore struct {
	defs []mcpstore.ServerDef
	err  error
}

func (s *fakeMCPStore) Add(context.Context, *int64, mcpstore.ServerDef) error { return nil }
func (s *fakeMCPStore) Remove(context.Context, *int64, string) error          { return nil }
func (s *fakeMCPStore) SetEnabled(context.Context, *int64, string, bool) error {
	return nil
}

func (s *fakeMCPStore) ListForProject(context.Context, int64) ([]mcpstore.ServerDef, error) {
	return s.defs, s.err
}

func (s *fakeMCPStore) ListAll(context.Context, int64) ([]mcpstore.ServerDef, []mcpstore.ServerDef, error) {
	return nil, s.defs, s.err
}

func TestResolveMCPServersExpandsSecrets(t *testing.T) {
	store := &fakeMCPStore{defs: []mcpstore.ServerDef{{
		Name:    "tavily",
		Command: "npx",
		Args:    []string{"-y", "tavily-mcp"},
		Env:     map[string]string{"TAVILY_API_KEY": "${TAVILY_KEY}", "SITE": "eu"},
		Enabled: true,
	}}}

	got := resolveMCPServers(context.Background(), store, config.Secrets{"TAVILY_KEY": "tv-secret"}, 1)

	require.Len(t, got, 1)
	assert.Equal(t, mcp.ServerConfig{
		Command: "npx",
		Args:    []string{"-y", "tavily-mcp"},
		Env:     map[string]string{"TAVILY_API_KEY": "tv-secret", "SITE": "eu"},
	}, got["tavily"])
}

// One server missing a secret must not take the whole session's MCP set down.
func TestResolveMCPServersSkipsUnresolvedReferences(t *testing.T) {
	store := &fakeMCPStore{defs: []mcpstore.ServerDef{
		{Name: "broken", Command: "run", Env: map[string]string{"K": "${MISSING}"}, Enabled: true},
		{Name: "fine", Command: "run", Env: map[string]string{"K": "${PRESENT}"}, Enabled: true},
	}}

	got := resolveMCPServers(context.Background(), store, config.Secrets{"PRESENT": "value"}, 1)

	require.Len(t, got, 1)
	assert.Contains(t, got, "fine")
	assert.NotContains(t, got, "broken")
}

func TestResolveMCPServersDegradesQuietly(t *testing.T) {
	tests := []struct {
		name      string
		store     mcpstore.Store
		projectID int64
	}{
		{name: "no registry wired", store: nil, projectID: 1},
		{name: "no project", store: &fakeMCPStore{}, projectID: 0},
		{name: "registry read fails", store: &fakeMCPStore{err: errors.New("db down")}, projectID: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Empty(t, resolveMCPServers(context.Background(), tt.store, nil, tt.projectID))
		})
	}
}

func TestResolveMCPServersLeavesEnvNilWhenEmpty(t *testing.T) {
	store := &fakeMCPStore{defs: []mcpstore.ServerDef{{Name: "bare", Command: "run", Enabled: true}}}

	got := resolveMCPServers(context.Background(), store, nil, 1)
	require.Len(t, got, 1)
	assert.Empty(t, got["bare"].Env)
}

// newMCPTestStore opens a migrated temp DB and returns a real registry store.
func newMCPTestStore(t *testing.T) (mcpstore.Store, *sql.DB, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	res, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "p")
	require.NoError(t, err)

	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return mcpstore.NewStore(db), db, projectID
}

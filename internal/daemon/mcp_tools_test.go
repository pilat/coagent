package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func newMCPToolSet(t *testing.T) (map[string]tool.Tool, mcpstore.Store, *recordingPool, int64) {
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

	store := mcpstore.NewStore(db)
	pool := &recordingPool{}

	tools := make(map[string]tool.Tool)
	for _, tl := range newMCPTools(store, pool, projectID) {
		tools[tl.ID()] = tl
	}

	return tools, store, pool, projectID
}

func run(t *testing.T, tl tool.Tool, params string) (*tool.Result, error) {
	t.Helper()

	return tl.Execute(context.Background(), json.RawMessage(params))
}

func TestMCPAddWritesToTheRequestedScope(t *testing.T) {
	tools, store, _, projectID := newMCPToolSet(t)
	ctx := context.Background()

	res, err := run(t, tools[tool.IDMCPAdd],
		`{"name":"tavily","scope":"project","command":"npx","args":["-y","tavily-mcp"],
		  "env":{"TAVILY_API_KEY":"${TAVILY_KEY}"}}`)
	require.NoError(t, err)
	assert.Contains(t, res.Output, "next run")

	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"ddg","scope":"global","command":"ddg-mcp"}`)
	require.NoError(t, err)

	globals, project, err := store.ListAll(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, globals, 1)
	require.Len(t, project, 1)
	assert.Equal(t, "ddg", globals[0].Name)
	assert.Equal(t, "tavily", project[0].Name)
	assert.Equal(t, map[string]string{"TAVILY_API_KEY": "${TAVILY_KEY}"}, project[0].Env,
		"references are stored literally and resolved at acquire time")
}

func TestMCPAddRejectsBadInput(t *testing.T) {
	tools, _, _, _ := newMCPToolSet(t)

	tests := []struct {
		name    string
		params  string
		wantErr string
	}{
		{name: "unknown scope", params: `{"name":"x","scope":"everywhere","command":"run"}`, wantErr: "scope must be"},
		{name: "missing scope", params: `{"name":"x","command":"run"}`, wantErr: "scope must be"},
		{name: "missing name", params: `{"scope":"global","command":"run"}`, wantErr: "required"},
		{name: "missing command", params: `{"name":"x","scope":"global"}`, wantErr: "required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(t, tools[tool.IDMCPAdd], tt.params)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestMCPAddRejectsDuplicateInSameScope(t *testing.T) {
	tools, _, _, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"global","command":"run"}`)
	require.NoError(t, err)

	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"global","command":"run"}`)
	require.ErrorIs(t, err, mcpstore.ErrDuplicate)

	// Shadowing a global with a project row is the documented override.
	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"project","command":"run"}`)
	require.NoError(t, err)
}

func TestMCPRegistryMutationsInvalidatePoolMetadata(t *testing.T) {
	tools, _, pool, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"tavily","scope":"global","command":"run"}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"tavily"}, pool.invalidated, "adding a server invalidates by name")

	for _, id := range []string{tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPRemove} {
		_, err = run(t, tools[id], `{"name":"tavily","scope":"global"}`)
		require.NoError(t, err)
		assert.Equal(t, []string{"tavily"}, pool.invalidated[len(pool.invalidated)-1:],
			"every successful mutation invalidates by name")
	}
}

func TestMCPFailedMutationInvalidatesNothing(t *testing.T) {
	tools, _, pool, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPDisable], `{"name":"ghost","scope":"global"}`)
	require.ErrorIs(t, err, mcpstore.ErrNotFound)
	assert.Empty(t, pool.invalidated, "a failed mutation must not touch pool metadata")
}

func TestMCPMutationsOnTheWrongScopeSayWhereItLives(t *testing.T) {
	tools, _, _, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"only-global","scope":"global","command":"run"}`)
	require.NoError(t, err)

	for _, id := range []string{tool.IDMCPRemove, tool.IDMCPDisable, tool.IDMCPEnable} {
		t.Run(id, func(t *testing.T) {
			_, err := run(t, tools[id], `{"name":"only-global","scope":"project"}`)
			require.ErrorIs(t, err, mcpstore.ErrNotFound)
			assert.Contains(t, err.Error(), "exists in global scope")
		})
	}
}

func TestMCPListShowsBothScopesAndStatusWithoutEnvValues(t *testing.T) {
	tools, _, _, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd],
		`{"name":"tavily","scope":"project","command":"npx","args":["-y","tavily-mcp"],
		  "env":{"TAVILY_API_KEY":"super-secret-value"}}`)
	require.NoError(t, err)

	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"ddg","scope":"global","command":"ddg-mcp"}`)
	require.NoError(t, err)
	_, err = run(t, tools[tool.IDMCPDisable], `{"name":"ddg","scope":"global"}`)
	require.NoError(t, err)

	res, err := run(t, tools[tool.IDMCPList], `{}`)
	require.NoError(t, err)

	assert.Contains(t, res.Output, "ddg [disabled]: ddg-mcp")
	assert.Contains(t, res.Output, "tavily [enabled]: npx -y tavily-mcp")
	assert.Contains(t, res.Output, "env: TAVILY_API_KEY")
	assert.NotContains(t, res.Output, "super-secret-value", "env values must never be rendered")
}

func TestMCPListOnAnEmptyRegistry(t *testing.T) {
	tools, _, _, _ := newMCPToolSet(t)

	res, err := run(t, tools[tool.IDMCPList], `{}`)
	require.NoError(t, err)
	assert.Contains(t, res.Output, "Global:\n  (none)")
	assert.Contains(t, res.Output, "This project:\n  (none)")
}

func TestMCPProjectScopeNeedsAProject(t *testing.T) {
	tools := make(map[string]tool.Tool)
	for _, tl := range newMCPTools(&fakeRegistryStore{}, nil, 0) {
		tools[tl.ID()] = tl
	}

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"x","scope":"project","command":"run"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no project")
}

// Every tool schema is a hand-written JSON literal with interpolated descriptions,
// so a malformed one would only surface as a provider 400 at runtime.
func TestMCPToolSchemasAreValidJSON(t *testing.T) {
	for _, tl := range newMCPTools(&fakeRegistryStore{}, nil, 1) {
		t.Run(tl.ID(), func(t *testing.T) {
			var schema map[string]any
			require.NoError(t, json.Unmarshal(tl.Parameters(), &schema))
			assert.Equal(t, "object", schema["type"])
			assert.NotEmpty(t, tl.Description())
		})
	}
}

// recordingPool is a Pool that only tracks invalidation; the tools use nothing else.
type recordingPool struct {
	stubPool

	invalidated []string
}

func (p *recordingPool) Invalidate(name string) { p.invalidated = append(p.invalidated, name) }

type fakeRegistryStore struct{ mcpstore.Store }

// stubPool satisfies the pool contract for tests that only care about invalidation.
type stubPool struct{}

func (stubPool) Acquire(context.Context, map[string]mcp.ServerConfig) (*mcp.Snapshot, error) {
	return nil, nil
}

func (stubPool) Release([]string) {}
func (stubPool) Stop()            {}
func (stubPool) ClientFor(context.Context, string, mcp.ServerConfig) (*mcp.Client, error) {
	return nil, nil
}
func (stubPool) Invalidate(string) {}

// A subagent must not reshape the toolset its parent will run with, so the
// registry tools are root-only.
func TestRegisterMCPToolsIsRootOnly(t *testing.T) {
	tests := []struct {
		name     string
		parentID int64
		store    mcpstore.Store
		want     []string
	}{
		{
			name:  "root session gets them",
			store: &fakeRegistryStore{},
			want:  []string{tool.IDMCPAdd, tool.IDMCPRemove, tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPList},
		},
		{name: "subagent does not", parentID: 7, store: &fakeRegistryStore{}},
		{name: "no registry wired", store: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &recordingSession{}
			s := &svc{mcpStore: tt.store}

			s.registerMCPTools(context.Background(),
				&sessionstore.SessionRecord{ID: 1, ParentID: tt.parentID, ProjectID: 3}, sess)

			assert.Equal(t, tt.want, sess.registered)
		})
	}
}

// recordingSession records which tools the daemon offers to a live session.
type recordingSession struct {
	session.Service

	registered []string
}

func (s *recordingSession) RegisterGatedTool(t tool.Tool) bool {
	s.registered = append(s.registered, t.ID())

	return true
}

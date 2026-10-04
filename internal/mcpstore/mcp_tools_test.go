package mcpstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/tool"
)

func newMCPToolSet(t *testing.T) (map[string]tool.Tool, Store, int64) {
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

	store := NewStore(db)
	tools := make(map[string]tool.Tool)
	for _, tl := range NewTools(store, projectID) {
		tools[tl.ID()] = tl
	}

	return tools, store, projectID
}

func run(t *testing.T, tl tool.Tool, params string) (*tool.Result, error) {
	t.Helper()

	return tl.Execute(context.Background(), json.RawMessage(params))
}

func TestMCPAddWritesToTheRequestedScope(t *testing.T) {
	tools, store, projectID := newMCPToolSet(t)
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
	tools, _, _ := newMCPToolSet(t)

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
	tools, _, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"global","command":"run"}`)
	require.NoError(t, err)

	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"global","command":"run"}`)
	require.ErrorIs(t, err, ErrDuplicate)

	// Shadowing a global with a project row is the documented override.
	_, err = run(t, tools[tool.IDMCPAdd], `{"name":"dup","scope":"project","command":"run"}`)
	require.NoError(t, err)
}

func TestMCPRegistryMutationsApplyOnNextRun(t *testing.T) {
	tools, store, projectID := newMCPToolSet(t)
	ctx := context.Background()

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"tavily","scope":"global","command":"run"}`)
	require.NoError(t, err)
	global, _, err := store.ListAll(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, global, 1)
	assert.True(t, global[0].Enabled)

	_, err = run(t, tools[tool.IDMCPDisable], `{"name":"tavily","scope":"global"}`)
	require.NoError(t, err)
	global, _, err = store.ListAll(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, global, 1)
	assert.False(t, global[0].Enabled)

	_, err = run(t, tools[tool.IDMCPEnable], `{"name":"tavily","scope":"global"}`)
	require.NoError(t, err)
	global, _, err = store.ListAll(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, global, 1)
	assert.True(t, global[0].Enabled)

	_, err = run(t, tools[tool.IDMCPRemove], `{"name":"tavily","scope":"global"}`)
	require.NoError(t, err)
	global, _, err = store.ListAll(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, global)
}

func TestMCPFailedMutationLeavesRegistryUnchanged(t *testing.T) {
	tools, store, projectID := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPDisable], `{"name":"ghost","scope":"global"}`)
	require.ErrorIs(t, err, ErrNotFound)
	global, project, err := store.ListAll(context.Background(), projectID)
	require.NoError(t, err)
	assert.Empty(t, global)
	assert.Empty(t, project)
}

func TestMCPMutationsOnTheWrongScopeSayWhereItLives(t *testing.T) {
	tools, _, _ := newMCPToolSet(t)

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"only-global","scope":"global","command":"run"}`)
	require.NoError(t, err)

	for _, id := range []string{tool.IDMCPRemove, tool.IDMCPDisable, tool.IDMCPEnable} {
		t.Run(id, func(t *testing.T) {
			_, err := run(t, tools[id], `{"name":"only-global","scope":"project"}`)
			require.ErrorIs(t, err, ErrNotFound)
			assert.Contains(t, err.Error(), "exists in global scope")
		})
	}
}

func TestMCPListShowsBothScopesAndStatusWithoutEnvValues(t *testing.T) {
	tools, _, _ := newMCPToolSet(t)

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
	tools, _, _ := newMCPToolSet(t)

	res, err := run(t, tools[tool.IDMCPList], `{}`)
	require.NoError(t, err)
	assert.Contains(t, res.Output, "Global:\n  (none)")
	assert.Contains(t, res.Output, "This project:\n  (none)")
}

func TestMCPProjectScopeNeedsAProject(t *testing.T) {
	tools := make(map[string]tool.Tool)
	for _, tl := range NewTools(&fakeRegistryStore{}, 0) {
		tools[tl.ID()] = tl
	}

	_, err := run(t, tools[tool.IDMCPAdd], `{"name":"x","scope":"project","command":"run"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no project")
}

// Every tool schema is a hand-written JSON literal with interpolated descriptions,
// so a malformed one would only surface as a provider 400 at runtime.
func TestMCPToolSchemasAreValidJSON(t *testing.T) {
	for _, tl := range NewTools(&fakeRegistryStore{}, 1) {
		t.Run(tl.ID(), func(t *testing.T) {
			var schema map[string]any
			require.NoError(t, json.Unmarshal(tl.Parameters(), &schema))
			assert.Equal(t, "object", schema["type"])
			assert.NotEmpty(t, tl.Description())
		})
	}
}

type fakeRegistryStore struct{ Store }

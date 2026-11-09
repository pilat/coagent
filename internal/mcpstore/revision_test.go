package mcpstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestRevisionMovesOnlyWhenTheRowChanges(t *testing.T) {
	for _, scope := range []string{"global", "project"} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()
			store, db, projectID := newTestStore(t)
			ctx := context.Background()
			var target *int64
			if scope == "project" {
				target = &projectID
			}
			require.NoError(t, store.Add(ctx, target, def("alpha")))
			require.NoError(t, store.Add(ctx, target, def("beta")))
			created := revisionOf(t, store, projectID, "alpha")
			neighbour := revisionOf(t, store, projectID, "beta")
			require.NoError(t, store.SetEnabled(ctx, target, "alpha", true))
			assert.Equal(
				t,
				created,
				revisionOf(t, store, projectID, "alpha"),
				"enabling an enabled row changes nothing",
			)
			require.NoError(t, store.SetEnabled(ctx, target, "alpha", false))
			off := revisionOf(t, store, projectID, "alpha")
			assert.NotEqual(t, created, off)
			require.NoError(t, store.SetEnabled(ctx, target, "alpha", false))
			assert.Equal(t, off, revisionOf(t, store, projectID, "alpha"), "disabling a disabled row changes nothing")
			require.NoError(t, store.SetEnabled(ctx, target, "alpha", true))
			on := revisionOf(t, store, projectID, "alpha")
			assert.NotEqual(t, created, on, "off and on must not restore the old revision")
			assert.NotEqual(t, off, on)
			require.NoError(t, store.Remove(ctx, target, "alpha"))
			require.NoError(t, store.Add(ctx, target, def("alpha")))
			assert.NotEqual(
				t,
				on,
				revisionOf(t, store, projectID, "alpha"),
				"an identical row added again is a new row",
			)
			assert.Equal(t, neighbour, revisionOf(t, store, projectID, "beta"))
			merged, err := store.ListForProject(ctx, projectID)
			require.NoError(t, err)
			require.Len(t, merged, 2)
			assert.NotEmpty(t, merged[0].Revision)
			var stored string
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT CAST(updated_at AS TEXT) FROM mcp_servers WHERE name = 'beta'`,
			).Scan(&stored))
			assert.Equal(t, neighbour, stored)
		})
	}
}

func TestRevisionOfShadowedGlobalLeavesEffectiveRowsUnchanged(t *testing.T) {
	t.Parallel()
	store, _, projectID := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.Add(ctx, nil, def("alpha")))
	require.NoError(t, store.Add(ctx, &projectID, def("alpha")))
	before, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.NoError(t, store.SetEnabled(ctx, nil, "alpha", false))
	require.NoError(t, store.SetEnabled(ctx, nil, "alpha", true))
	after, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestRevisionIsAbsentFromMCPList(t *testing.T) {
	t.Parallel()
	tools, store, projectID := newMCPToolSet(t)
	require.NoError(t, store.Add(context.Background(), &projectID, def("alpha")))
	revision := revisionOf(t, store, projectID, "alpha")
	result, err := run(t, tools[tool.IDMCPList], `{}`)
	require.NoError(t, err)
	assert.NotContains(t, result.Output, revision)
	assert.NotContains(t, result.Output, "revision")
}

func revisionOf(t *testing.T, store Store, projectID int64, name string) string {
	t.Helper()
	globals, project, err := store.ListAll(context.Background(), projectID)
	require.NoError(t, err)
	for _, row := range append(globals, project...) {
		if row.Name == name {
			require.NotEmpty(t, row.Revision)
			return row.Revision
		}
	}
	require.Failf(t, "row missing", "%q", name)
	return ""
}

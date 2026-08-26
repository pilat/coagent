package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_ClaimLegacyCLIRootCreatesOwnerAndLifecycleTogether(t *testing.T) {
	ctx := context.Background()
	store, db, _ := newTestStore(t)
	projectDir := t.TempDir()
	project, err := db.ExecContext(
		ctx,
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		projectDir,
		"sys:coagent",
	)
	require.NoError(t, err)
	id, err := project.LastInsertId()
	require.NoError(t, err)
	root, err := store.CreateSession(ctx, id, "model", "", map[string]any{"channel": "cli"})
	require.NoError(t, err)
	claimer, ok := store.(LegacyCLIClaimStore)
	require.True(t, ok)
	require.NoError(t, claimer.ClaimLegacyCLIRoots(ctx, "sys:coagent", projectDir, "cli", "cli"))

	record, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, "cli", record.Attributes[managerIDAttribute])
	var kind, owner string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT type, json_extract(attributes, '$.manager_id')
		FROM session_outbox WHERE session_id = ?`, root.ID).Scan(&kind, &owner))
	assert.Equal(t, string(OutputSessionOpened), kind)
	assert.Equal(t, "cli", owner)
}

func TestStore_ClaimLegacyCLIRootRejectsSameNameOutsideCanonicalProject(t *testing.T) {
	ctx := context.Background()
	store, db, _ := newTestStore(t)
	foreignDir := t.TempDir()
	project, err := db.ExecContext(
		ctx,
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		foreignDir,
		"sys:coagent",
	)
	require.NoError(t, err)
	projectID, err := project.LastInsertId()
	require.NoError(t, err)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"channel": "cli"})
	require.NoError(t, err)
	claimer := store.(LegacyCLIClaimStore)
	require.NoError(t, claimer.ClaimLegacyCLIRoots(ctx, "sys:coagent", t.TempDir(), "cli", "cli"))

	record, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.NotContains(t, record.Attributes, managerIDAttribute)
}

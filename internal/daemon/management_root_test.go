package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// Three managers share one hidden project but own three independent roots,
// each bound to its own service topic.
func TestManagementRoot_ThreeManagersShareProjectKeepOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "mgmt.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	projects := NewStore(db)
	sessions := sessionstore.NewStore(db)
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc, _ := newSvc(
		ctx, &mockFactory{}, projects, sessions, sessions, sessions,
		sessions, sessions, sessions, sessions,
		subagent.NewStore(db), subagent.NewTransactions(db),
		nil, nil, nil, func() string { return "fake-model" },
	)
	factory := newTestController(svc, cfg, nil, nil)

	const topicBase = 7000

	roots := make(map[string]int64)

	for i, managerID := range []string{"tg-one", "tg-two", "tg-three"} {
		id, err := factory.ForManager(managerID).EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
			TopicID: int64(topicBase + i),
		})
		require.NoError(t, err)
		require.NotZero(t, id)
		roots[managerID] = id
	}

	assert.Len(t, roots, 3, "three managers own three distinct roots")

	projectIDs := map[int64]bool{}

	for managerID, id := range roots {
		record, err := sessions.GetSession(ctx, id)
		require.NoError(t, err)
		projectIDs[record.ProjectID] = true
		assert.Equal(t, managerID, record.Attributes[controllerapi.SessionAttributeManagerID])
		assert.Contains(t, record.Attributes, controllerapi.SessionAttributeManagementSurface)
	}

	assert.Len(t, projectIDs, 1, "all management roots share one hidden project")

	var hidden bool

	for projectID := range projectIDs {
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT hidden FROM projects WHERE id = ?`, projectID).Scan(&hidden))
		assert.True(t, hidden, "the shared management project is hidden")

		var name string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT name FROM projects WHERE id = ?`, projectID).Scan(&name))
		assert.Equal(t, controllerapi.CoagentManagementProjectDir, name)
	}
}

// Restart reconciliation resumes the same live root and patches a recreated
// service topic without inserting a duplicate.
func TestManagementRoot_RestartResumesSameRootAndPatchesTopic(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "mgmt.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	projects := NewStore(db)
	sessions := sessionstore.NewStore(db)
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc, _ := newSvc(
		ctx, &mockFactory{}, projects, sessions, sessions, sessions,
		sessions, sessions, sessions, sessions,
		subagent.NewStore(db), subagent.NewTransactions(db),
		nil, nil, nil, func() string { return "fake-model" },
	)
	factory := newTestController(svc, cfg, nil, nil)

	first, err := factory.ForManager("tg-main").EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
		TopicID: 7001,
	})
	require.NoError(t, err)

	second, err := factory.ForManager("tg-main").EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
		TopicID: 7002,
	})
	require.NoError(t, err)
	assert.Equal(t, first, second, "restart with a recreated topic resumes the same root")

	record, err := sessions.GetSession(ctx, second)
	require.NoError(t, err)
	raw, ok := record.Attributes[controllerapi.SessionAttributeTelegramTopicID]
	require.True(t, ok, "the topic binding is patched to the current service topic")
	assert.Equal(t, int64(7002), topicAttrNumber(raw))
}

func topicAttrNumber(raw any) int64 {
	switch v := raw.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

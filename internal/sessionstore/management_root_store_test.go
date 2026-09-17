package sessionstore

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureManagementRoot_RepeatedEnsureReturnsSameRoot(t *testing.T) {
	st, _, projectID := newTestStore(t)
	ctx := context.Background()

	first, commit, err := st.EnsureManagementRoot(
		ctx,
		projectID,
		"tg-main",
		7001,
		"sys_coagent management",
		"/tmp/mgmt",
	)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, commit, "first ensure writes the session_opened lifecycle row")

	second, commit, err := st.EnsureManagementRoot(
		ctx,
		projectID,
		"tg-main",
		7001,
		"sys_coagent management",
		"/tmp/mgmt",
	)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Nil(t, commit, "repeated ensure must not write another lifecycle row")

	var count int
	require.NoError(t, st.(*store).db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND type = 'session_opened'`,
		first.ID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestEnsureManagementRoot_ConcurrentEnsureProducesOneRoot(t *testing.T) {
	st, _, projectID := newTestStore(t)
	ctx := context.Background()

	const callers = 8

	ids := make([]int64, callers)

	var wg sync.WaitGroup

	for i := range callers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			record, _, err := st.EnsureManagementRoot(
				ctx, projectID, "tg-main", 7001, "sys_coagent management", "/tmp/mgmt",
			)
			if err == nil {
				ids[i] = record.ID
			}
		}(i)
	}

	wg.Wait()

	for _, id := range ids {
		require.NotZero(t, id, "every concurrent ensure must return the root")
		assert.Equal(t, ids[0], id)
	}

	var roots int
	require.NoError(t, st.(*store).db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE project_id = ? AND parent_id = 0 AND killed_at IS NULL
			AND status NOT IN ('terminating', 'killed')`, projectID).Scan(&roots))
	assert.Equal(t, 1, roots)

	var opened int
	require.NoError(t, st.(*store).db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE type = 'session_opened'`).Scan(&opened))
	assert.Equal(t, 1, opened)
}

func TestEnsureManagementRoot_OwnersGetSeparateRoots(t *testing.T) {
	st, _, projectID := newTestStore(t)
	ctx := context.Background()

	first, _, err := st.EnsureManagementRoot(ctx, projectID, "tg-one", 7001, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)

	second, _, err := st.EnsureManagementRoot(ctx, projectID, "tg-two", 7002, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, projectID, second.ProjectID)
}

func TestEnsureManagementRoot_StaleTopicBindingIsPatched(t *testing.T) {
	st, _, projectID := newTestStore(t)
	ctx := context.Background()

	first, _, err := st.EnsureManagementRoot(ctx, projectID, "tg-main", 7001, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)

	second, commit, err := st.EnsureManagementRoot(
		ctx,
		projectID,
		"tg-main",
		7002,
		"sys_coagent management",
		"/tmp/mgmt",
	)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Nil(t, commit, "a topic patch writes no lifecycle row")
	assert.Equal(t, int64(7002), topicNumber(second.Attributes[telegramTopicAttribute]))
}

func TestEnsureManagementRoot_ClearReplacementAllowsSuccessor(t *testing.T) {
	st, _, projectID := newTestStore(t)
	ctx := context.Background()

	old, _, err := st.EnsureManagementRoot(ctx, projectID, "tg-main", 7001, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)

	_, _, err = st.ReplaceManagerRoot(ctx, old.ID, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)

	next, _, err := st.EnsureManagementRoot(ctx, projectID, "tg-main", 7001, "sys_coagent management", "/tmp/mgmt")
	require.NoError(t, err)
	assert.NotEqual(t, old.ID, next.ID, "the terminating root leaves the uniqueness set")
}

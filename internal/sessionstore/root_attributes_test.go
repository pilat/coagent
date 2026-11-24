package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateRootWithoutAttributesStoresAnEmptyObject(t *testing.T) {
	store, db, project := newTestStore(t)
	root, _, err := store.CreateRoot(t.Context(), RootCreate{
		ProjectID: project, Model: "model", Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	var attributes string
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT attributes FROM sessions WHERE id = ?", root.ID).Scan(&attributes))
	assert.Equal(t, "{}", attributes)
}

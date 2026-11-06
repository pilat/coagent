package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatOutputs_DisplayNameSurvivesKill(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, root.ID, "answer")
	for _, title := range []string{"", "chosen title"} {
		if title != "" {
			require.NoError(t, store.SetSessionTitle(ctx, "owner", root.ID, title))
		}
		rows, err := store.ChatOutputs(ctx, root.ID, 0, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		want := "test"
		if title != "" {
			want += " · " + title
		}
		assert.Equal(t, want, rows[0].Name)
	}
	_, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 0)
	require.NoError(t, err)
	rows, err := store.ChatOutputs(ctx, root.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, "test · chosen title", row.Name)
	}
}

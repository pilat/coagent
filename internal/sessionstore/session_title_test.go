package sessionstore

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestSessionTitle_FirstAcceptedInput(t *testing.T) {
	for _, tt := range []struct{ name, content, want string }{
		{"plain", " fix login ", "fix login"},
		{"command", "/skill foo", ""},
		{"empty", "", ""},
		{"whitespace", " \t\n", ""},
		{"multiline", "first\r\nsecond", "first"},
		{"80 runes", strings.Repeat("界", 80), strings.Repeat("界", 80)},
		{"81 runes", strings.Repeat("界", 81), strings.Repeat("界", 79) + "…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, _, project := newTestStore(t)
			root := chatTestRoot(t, store, project)
			_, err := store.Enqueue(
				t.Context(),
				Input{SessionID: root.ID, Source: InputSourceUser, Content: tt.content},
			)
			if tt.content == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assertSessionTitle(t, store, root.ID, tt.want)
			_, err = store.Enqueue(t.Context(), Input{SessionID: root.ID, Source: InputSourceUser, Content: "next"})
			require.NoError(t, err)
			want := tt.want
			if want == "" {
				want = "next"
			}
			assertSessionTitle(t, store, root.ID, want)
		})
	}
}

func TestSessionTitle_ExcludedAndDuplicateInputs(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project)
	child, err := store.CreateSubagentSession(t.Context(), project, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	ownerless := sessionstoretest.BareRoot(t, store.db, project, "model", "", nil)
	for _, id := range []int64{child, ownerless} {
		_, err = store.Enqueue(t.Context(), Input{SessionID: id, Source: InputSourceUser, Content: "no title"})
		require.NoError(t, err)
	}
	assertSessionTitle(t, store, child, "")
	assertSessionTitle(t, store, ownerless, "no title")
	_, err = store.Enqueue(t.Context(), Input{SessionID: root.ID, Source: InputSourceSchedule, Content: "scheduled"})
	require.NoError(t, err)
	assertSessionTitle(t, store, root.ID, "")
	_, err = store.Enqueue(
		t.Context(),
		Input{SessionID: root.ID, Source: InputSourceUser, Content: "/model x", DeliveryKey: "same"},
	)
	require.NoError(t, err)
	result, err := store.Enqueue(
		t.Context(),
		Input{SessionID: root.ID, Source: InputSourceUser, Content: "duplicate", DeliveryKey: "same"},
	)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assertSessionTitle(t, store, root.ID, "")
}

func TestSessionTitle_CreateAndClear(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	for _, title := range []string{"", "chosen"} {
		attrs := map[string]any{}
		if title != "" {
			attrs[TitleAttribute] = title
		}
		root, _, err := store.CreateRoot(t.Context(), RootCreate{
			ProjectID:  project,
			Name:       "test",
			WorkDir:    "/project",
			Model:      "model",
			Attributes: attrs,
			Input:      Input{Source: InputSourceUser, Content: "first request"},
		})
		require.NoError(t, err)
		want := title
		if want == "" {
			want = "first request"
		}
		assertSessionTitle(t, store, root.ID, want)
		input, err := store.Enqueue(t.Context(), Input{SessionID: root.ID, Source: InputSourceUser, Content: "/clear"})
		require.NoError(t, err)
		replacement, _, err := store.ReplaceRootForInput(
			t.Context(),
			root.ID,
			input.Input.ID,
			"test",
			"/project",
		)
		require.NoError(t, err)
		assert.NotContains(t, replacement.Attributes, TitleAttribute)
		assertSessionTitle(t, store, replacement.ID, "")
		_, err = store.Enqueue(
			t.Context(),
			Input{SessionID: replacement.ID, Source: InputSourceUser, Content: "new request"},
		)
		require.NoError(t, err)
		assertSessionTitle(t, store, replacement.ID, "new request")
	}
}

func TestSessionTitle_RenameAuthorityAndOrder(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	id := sessionstoretest.BareRoot(t, store.db, project, "model", "", map[string]any{"channel": "telegram"})
	root, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, store.SetSessionTitle(t.Context(), root.ID, " chosen "))
	assertSessionTitle(t, store, root.ID, "chosen")
	updated, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Equal(t, root.UpdatedAt, updated.UpdatedAt)
	assert.Equal(t, "telegram", updated.Attributes["channel"])
	for _, raw := range []string{"", strings.Repeat("界", 81), "two\nlines", "two\rlines"} {
		require.EqualError(
			t,
			store.SetSessionTitle(t.Context(), root.ID, raw),
			"title must be one line of 1-80 characters",
		)
	}
	child, err := store.CreateSubagentSession(t.Context(), project, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	require.Error(t, store.SetSessionTitle(t.Context(), child, "foreign"))
	_, err = store.MarkSessionKilledWithOutput(t.Context(), root.ID, 0)
	require.NoError(t, err)
	require.Error(t, store.SetSessionTitle(t.Context(), root.ID, "killed"))
	assertSessionTitle(t, store, root.ID, "chosen")
}

func assertSessionTitle(t *testing.T, store *Store, id int64, want string) {
	t.Helper()
	var title sql.NullString
	require.NoError(
		t,
		store.db.QueryRowContext(t.Context(), `SELECT json_extract(attributes, '$.title') FROM sessions WHERE id = ?`, id).
			Scan(&title),
	)
	assert.Equal(t, want, title.String)
	assert.Equal(t, want != "", title.Valid)
}

package sessionstore

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadWatch_PagesAndPositions(t *testing.T) {
	t.Run("classifies kinds", func(t *testing.T) {
		store, _, project := newTestStore(t)
		root, _, err := store.CreateManagerRoot(t.Context(), ManagerRootCreate{
			ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
			Name: "test", WorkDir: "/project",
		})
		require.NoError(t, err)
		watcher := chatTestRoot(t, store, project, "owner")
		keys := []string{
			"message:1:reply", "message:2:final", "input:3:sent", "schedule:x:announcement",
			"input:4:status:result", "",
		}
		for _, key := range keys {
			watchTestOutput(t, store, root.ID, key, key+" content")
		}
		_, err = store.MarkSessionKilledWithOutput(t.Context(), root.ID, 0)
		require.NoError(t, err)
		page, err := store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		var kinds []string
		for _, row := range page.Rows {
			kinds = append(kinds, row.Kind)
			assert.False(t, row.CreatedAt.IsZero())
		}
		assert.Equal(t, []string{"step", "answer", "sent", "scheduled", "notice", "notice", "closed"}, kinds)
		assert.Equal(t, "test", page.Name)
	})

	t.Run("first read is latest", func(t *testing.T) {
		store, _, project := newTestStore(t)
		root := chatTestRoot(t, store, project, "owner")
		watcher := chatTestRoot(t, store, project, "owner")
		for i := range 5 {
			watchTestOutput(t, store, root.ID, fmt.Sprintf("message:%d:reply", i), fmt.Sprint(i))
		}
		read := WatchRead{WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 2, MaxRunes: 16000}
		page, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"3", "4"}, watchContents(page))
		assert.Equal(t, 3, page.Skipped)
		assert.Zero(t, page.Remaining)
		page, err = store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Empty(t, page.Rows)
		for i := range 3 {
			watchTestOutput(t, store, root.ID, fmt.Sprintf("message:%d:final", i+5), fmt.Sprint(i+5))
		}
		page, err = store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"5", "6"}, watchContents(page))
		assert.Equal(t, 1, page.Remaining)
		assert.Equal(t, 1, page.RemainingAnswers)
		assert.Zero(t, page.Skipped)
	})

	t.Run("empty first read stays latest", func(t *testing.T) {
		store, db, project := newTestStore(t)
		root := chatTestRoot(t, store, project, "owner")
		watcher := chatTestRoot(t, store, project, "owner")
		read := WatchRead{WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 2, MaxRunes: 16000}
		page, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Empty(t, page.Rows)
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM session_watches WHERE watcher_id = ? AND target_id = ?`,
			watcher.ID, root.ID).Scan(&count))
		assert.Zero(t, count)
		for i := range 5 {
			watchTestOutput(t, store, root.ID, "", fmt.Sprint(i))
		}
		page, err = store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"3", "4"}, watchContents(page))
		assert.Equal(t, 3, page.Skipped)
		assert.Zero(t, page.Remaining)
	})

	t.Run("rune bound", func(t *testing.T) {
		for _, latest := range []bool{false, true} {
			store, _, project := newTestStore(t)
			root := chatTestRoot(t, store, project, "owner")
			watcher := chatTestRoot(t, store, project, "owner")
			read := WatchRead{WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 30, MaxRunes: 25}
			watchTestOutput(t, store, root.ID, "", "already read")
			_, err := store.ReadWatch(t.Context(), read)
			require.NoError(t, err)
			for range 3 {
				watchTestOutput(t, store, root.ID, "", strings.Repeat("ж", 10))
			}
			read.Latest = latest
			page, err := store.ReadWatch(t.Context(), read)
			require.NoError(t, err)
			require.Len(t, page.Rows, 2)
			assert.Equal(t, 10, utf8.RuneCountInString(page.Rows[0].Content))
			read.Latest = true
			watchTestOutput(t, store, root.ID, "", strings.Repeat("ж", 40))
			page, err = store.ReadWatch(t.Context(), read)
			require.NoError(t, err)
			require.Len(t, page.Rows, 1)
			assert.Equal(t, strings.Repeat("ж", 25), page.Rows[0].Content)
			assert.Equal(t, 15, page.Rows[0].Clipped)
		}
	})

	t.Run("latest flag", func(t *testing.T) {
		store, _, project := newTestStore(t)
		root := chatTestRoot(t, store, project, "owner")
		watcher := chatTestRoot(t, store, project, "owner")
		read := WatchRead{WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 1, MaxRunes: 16000}
		watchTestOutput(t, store, root.ID, "", "already read")
		_, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		for i := range 4 {
			watchTestOutput(t, store, root.ID, "", fmt.Sprint(i))
		}
		read.Latest = true
		page, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"3"}, watchContents(page))
		assert.Equal(t, 3, page.Skipped)
		read.Latest = false
		page, err = store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Empty(t, page.Rows)
	})

	t.Run("chain", func(t *testing.T) {
		store, db, project := newTestStore(t)
		first := chatTestRoot(t, store, project, "owner")
		second := chatTestRoot(t, store, project, "owner")
		watcher := chatTestRoot(t, store, project, "owner")
		read := WatchRead{WatcherID: watcher.ID, Chain: []int64{first.ID}, MaxRows: 30, MaxRunes: 16000}
		watchTestOutput(t, store, first.ID, "", "already read")
		_, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		watchTestOutput(t, store, first.ID, "", "old answer")
		watchTestOutput(t, store, second.ID, "", "new answer")
		read.Chain = []int64{first.ID, second.ID}
		page, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"old answer", "new answer"}, watchContents(page))
		var target, count int64
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT target_id, COUNT(*) FROM session_watches WHERE watcher_id = ?`, watcher.ID).Scan(&target, &count))
		assert.Equal(t, second.ID, target)
		assert.EqualValues(t, 1, count)
	})

	t.Run("position is per watcher and survives reopen", func(t *testing.T) {
		store, db, project := newTestStore(t)
		root := chatTestRoot(t, store, project, "owner")
		first := chatTestRoot(t, store, project, "owner")
		second := chatTestRoot(t, store, project, "owner")
		last := watchTestOutput(t, store, root.ID, "", "first")
		read := WatchRead{WatcherID: first.ID, Chain: []int64{root.ID}, MaxRows: 1, MaxRunes: 16000}
		_, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		var position int64
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT read_through FROM session_watches WHERE watcher_id = ?`, first.ID).Scan(&position))
		assert.Equal(t, last, position)
		store = NewStore(db)
		watchTestOutput(t, store, root.ID, "", "second")
		watchTestOutput(t, store, root.ID, "", "third")
		page, err := store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"second"}, watchContents(page))
		read.WatcherID = second.ID
		page, err = store.ReadWatch(t.Context(), read)
		require.NoError(t, err)
		assert.Equal(t, []string{"third"}, watchContents(page))
		assert.Equal(t, 2, page.Skipped)
	})

	t.Run("never touches chat focus", func(t *testing.T) {
		store, _, project := newTestStore(t)
		root := chatTestRoot(t, store, project, "owner")
		watcher := chatTestRoot(t, store, project, "owner")
		id := watchTestOutput(t, store, root.ID, "", "answer")
		require.NoError(t, store.BindChat(t.Context(), "owner", "chat"))
		require.NoError(t, store.SetChatFocus(t.Context(), "owner", "chat", root.ID))
		ok, err := store.EnterChat(t.Context(), "owner", "chat", root.ID, id)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher.ID, Chain: []int64{root.ID}, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		focus, cursor, entered, err := store.ChatCursor(t.Context(), "owner", "chat")
		require.NoError(t, err)
		assert.Equal(t, root.ID, focus)
		assert.Equal(t, id, cursor)
		assert.True(t, entered)
	})

	t.Run("validation", func(t *testing.T) {
		store, _, _ := newTestStore(t)
		for _, read := range []WatchRead{
			{MaxRows: 1, MaxRunes: 1}, {Chain: []int64{1}, MaxRunes: 1}, {Chain: []int64{1}, MaxRows: 1},
		} {
			_, err := store.ReadWatch(t.Context(), read)
			require.Error(t, err)
		}
	})
}

func watchTestOutput(t *testing.T, store *Store, id int64, key, content string) int64 {
	t.Helper()
	attrs := map[string]any{}
	if strings.HasPrefix(key, "input:") && strings.HasSuffix(key, ":sent") {
		attrs["source"] = "agent"
	}
	if strings.HasPrefix(key, "schedule:") {
		attrs["source"] = "scheduler"
	}
	out, err := store.EnqueueOutput(t.Context(), OutputDraft{
		SessionID: id, Type: OutputMessage, SourceKey: key, Content: content, Attributes: attrs,
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	return out.OutputID
}

func watchContents(page *WatchPage) []string {
	var contents []string
	for _, row := range page.Rows {
		contents = append(contents, row.Content)
	}
	return contents
}

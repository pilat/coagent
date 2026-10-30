package sessionstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatSessions_ActivityGroupsAndRename(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, db, project := newTestStore(t)
	assistant, err := store.EnsureAssistantRoot(ctx, project, "owner", "/assistant", "model", "")
	require.NoError(t, err)
	var roots []*SessionRecord
	for range 4 {
		roots = append(roots, chatTestRoot(t, store, project, "owner"))
	}
	base := time.Date(2026, time.October, 30, 10, 0, 0, 0, time.UTC)
	for i, root := range roots {
		output := chatTestOutput(t, store, root.ID, "answer")
		_, err := db.ExecContext(ctx, `UPDATE session_outbox SET created_at = ? WHERE id = ?`,
			base.Add(time.Duration(i)*time.Hour), output)
		require.NoError(t, err)
		if i >= 2 {
			require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, output))
		}
	}
	want := []int64{0, roots[1].ID, roots[0].ID, roots[3].ID, roots[2].ID}
	for _, rename := range []bool{false, true} {
		if rename {
			require.NoError(t, store.SetSessionTitle(ctx, "owner", roots[0].ID, "renamed"))
		}
		sessions, err := store.ChatSessions(ctx, "owner", "chat")
		require.NoError(t, err)
		var ids []int64
		for _, session := range sessions {
			ids = append(ids, session.ID)
		}
		assert.Equal(t, want, ids)
		assert.Equal(t, assistant.ID, sessions[0].SessionID)
	}
}

func TestChatSessions_LastActivitySources(t *testing.T) {
	for _, tt := range []struct {
		name, source, attributes string
		output                   bool
		wantFirst                bool
	}{
		{name: "user", source: "user", wantFirst: true},
		{name: "assistant", source: "user", attributes: `{"from":"assistant"}`, wantFirst: true},
		{name: "agent", source: "agent", wantFirst: true},
		{name: "process", source: "process"},
		{name: "schedule", source: "schedule"},
		{name: "subagent", source: "subagent"},
		{name: "call result", source: "call_result"},
		{name: "lifecycle output", output: true, wantFirst: true},
		{name: "creation fallback", wantFirst: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, db, project := newTestStore(t)
			root := chatTestRoot(t, store, project, "owner")
			other := chatTestRoot(t, store, project, "owner")
			base := time.Date(2026, time.October, 30, 10, 0, 0, 0, time.UTC)
			_, err := db.ExecContext(ctx, `UPDATE sessions SET created_at = ?, updated_at = ?`, base, base)
			require.NoError(t, err)
			if tt.source != "" || tt.output {
				out := chatTestOutput(t, store, root.ID, "older output")
				_, err = db.ExecContext(ctx, `UPDATE session_outbox SET created_at = ? WHERE id = ?`, base, out)
				require.NoError(t, err)
				require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, out))
				out = chatTestOutput(t, store, other.ID, "other output")
				// SQLite and Go timestamp encodings must compare as times, not as text.
				_, err = db.ExecContext(ctx, `UPDATE session_outbox SET created_at = ? WHERE id = ?`,
					"2026-10-30T11:00:00Z", out)
				require.NoError(t, err)
				require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", other.ID, out))
			}
			switch {
			case tt.source != "":
				attrs := tt.attributes
				if attrs == "" {
					attrs = `{}`
				}
				_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
					(session_id, source, raw_content, attributes, received_at) VALUES (?, ?, 'input', ?, ?)`,
					root.ID, tt.source, attrs, base.Add(2*time.Hour))
			case tt.output:
				output, enqueueErr := store.EnqueueOutput(ctx, OutputDraft{
					SessionID: root.ID, Type: OutputSessionOpened,
					Attributes: map[string]any{"name": "test", "work_dir": "/project"},
				})
				require.NoError(t, enqueueErr)
				_, err = db.ExecContext(ctx, `UPDATE session_outbox SET created_at = ? WHERE id = ?`,
					base.Add(2*time.Hour), output.OutputID)
			default:
				_, err = db.ExecContext(ctx, `UPDATE sessions SET created_at = ? WHERE id = ?`,
					base.Add(2*time.Hour), root.ID)
			}
			require.NoError(t, err)
			sessions, err := store.ChatSessions(ctx, "owner", "chat")
			require.NoError(t, err)
			require.Len(t, sessions, 2)
			want := other.ID
			if tt.wantFirst {
				want = root.ID
			}
			assert.Equal(t, want, sessions[0].ID)
		})
	}
}

func TestChatSessions_ActivityTiesUseDescendingID(t *testing.T) {
	t.Parallel()
	store, db, project := newTestStore(t)
	first := chatTestRoot(t, store, project, "owner")
	second := chatTestRoot(t, store, project, "owner")
	_, err := db.ExecContext(t.Context(), `UPDATE sessions SET created_at = '2026-10-30 10:00:00'`)
	require.NoError(t, err)
	sessions, err := store.ChatSessions(t.Context(), "owner", "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, second.ID, sessions[0].ID)
	assert.Equal(t, first.ID, sessions[1].ID)
}

package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantToolsHandoverUserWords(t *testing.T) {
	for _, tc := range []struct {
		name, words, instruction string
		compacted                bool
		want                     string
	}{
		{"verbatim multiline", "  User's line one\r\n\r\nline three  ", "do the work", false, "> User's line one\n>\n> line three"},
		{"slash command", "do this", "/status", false, ""},
		{"already quoted", "do this", "Please do this carefully", false, ""},
		{"compacted user", "old user words", "do the work", true, ""},
		{"no active user", "", "do the work", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newToolFixture(t)
			if tc.words != "" {
				answered(t, f.sessions, f.root, tc.words, "previous answer")
			}
			if tc.compacted {
				_, err := f.db.ExecContext(
					t.Context(),
					`UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE session_id = ?`,
					f.root,
				)
				require.NoError(t, err)
			}
			for _, id := range []string{toolOpen, toolSend} {
				args, err := json.Marshal(
					map[string]any{"session": f.work, "project": "devbox", "about": "work", "text": tc.instruction},
				)
				require.NoError(t, err)
				f.execute(t, id, string(args))
				text := f.host.deliveries[len(f.host.deliveries)-1]
				if tc.want == "" {
					assert.Equal(t, tc.instruction, text)
				} else {
					assert.Contains(t, text, tc.want)
					assert.Contains(
						t,
						text,
						"The user's own words (they work with several sessions at once, so part of this may concern other work):",
					)
					assert.NotContains(t, text, "\r")
				}
				var stored string
				work := f.work
				if id == toolOpen {
					work = f.host.created[len(f.host.created)-1]
				}
				require.NoError(
					t,
					f.db.QueryRowContext(t.Context(), `SELECT raw_content FROM session_inbox WHERE session_id = ? ORDER BY id DESC LIMIT 1`, work).
						Scan(&stored),
				)
				assert.Equal(t, text, stored)
			}
		})
	}
}

func TestAssistantHandoverQuotesNewestActiveUserAcrossEvents(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	answered(t, f.sessions, f.root, "old request", "old answer")
	answered(t, f.sessions, f.root, "new request", "new answer")
	event, err := f.sessions.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: f.root, Source: sessionstore.InputSourceAgent, Content: "work answered"},
	)
	require.NoError(t, err)
	_, err = f.sessions.Commit(
		t.Context(),
		sessionstore.Commit{
			SessionID: f.root,
			Accept: []sessionstore.Accept{
				{
					InputID: event.Input.ID,
					State:   sessionstore.InputStateAccepted,
					Content: "work answered",
					LinkRef: -1,
				},
			},
		},
	)
	require.NoError(t, err)
	for range 2 {
		f.execute(t, toolSend, `{"session":`+itoa(f.work)+`,"text":"follow up"}`)
	}
	for _, text := range f.host.deliveries {
		assert.True(t, strings.HasSuffix(text, "> new request"))
		assert.NotContains(t, text, "old request")
		assert.NotContains(t, text, "work answered")
	}
}

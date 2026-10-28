package assistant

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAssistantDeskConversationFromInboxAndOutbox(t *testing.T) {
	t.Parallel()
	db, sessions, store, root := assistantFixture(t)
	now := time.Now().UTC()
	empty, err := store.LoadDesk(t.Context(), root, now)
	require.NoError(t, err)
	assert.Empty(t, empty.Conversation)
	assert.Zero(t, empty.Spoken)
	for i := range 25 {
		seedDeskExchange(t, db, sessions, root, i, now.Add(time.Duration(i)*time.Minute))
	}
	_, err = db.ExecContext(
		t.Context(),
		`UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE session_id = ?`,
		root,
	)
	require.NoError(t, err)
	_, err = sessions.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "unaccepted input"},
	)
	require.NoError(t, err)
	_, err = sessions.Commit(
		t.Context(),
		sessionstore.Commit{SessionID: root, At: now.Add(26 * time.Minute), Outputs: []sessionstore.Output{
			{
				Type:       sessionstore.OutputMessagePersistent,
				Key:        "host:error",
				Content:    "host error notice",
				MessageRef: -1,
			},
			{
				Type:       sessionstore.OutputMessageReplaceable,
				Key:        "host:compaction",
				Content:    "host compaction notice",
				MessageRef: -1,
			},
			{
				Type:       sessionstore.OutputMessagePersistent,
				Key:        "input:status",
				Content:    "host status receipt",
				MessageRef: -1,
			},
		}},
	)
	require.NoError(t, err)
	desk, err := store.LoadDesk(t.Context(), root, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 50, desk.Spoken)
	require.Len(t, desk.Conversation, 40)
	for i, line := range desk.Conversation {
		pair := 5 + i/2
		if i%2 == 0 {
			assert.Equal(t, userKind, line.Kind)
			assert.Equal(t, fmt.Sprintf("request %02d", pair), line.Text)
		} else {
			assert.Equal(t, sayKind, line.Kind)
			assert.Equal(t, fmt.Sprintf("spoken answer %02d", pair), line.Text)
		}
	}
	text := Render(View{Desk: desk}, now.Add(time.Hour))
	assert.Contains(t, text, "last 40 of 50")
	assert.NotContains(t, text, "host ")
	assert.NotContains(t, text, "confirming done")
	assert.NotContains(t, text, "unaccepted input")
	assert.NotContains(t, text, "## event")
}

func TestAssistantDeskUnansweredUserAndClipping(t *testing.T) {
	t.Parallel()
	db, sessions, store, root := assistantFixture(t)
	now := time.Now().UTC()
	seedDeskExchange(t, db, sessions, root, 0, now)
	words := strings.Repeat("я", 2500)
	input, err := sessions.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: words},
	)
	require.NoError(t, err)
	_, err = db.ExecContext(
		t.Context(),
		`UPDATE session_inbox SET received_at = ? WHERE id = ?`,
		now.Add(time.Hour),
		input.Input.ID,
	)
	require.NoError(t, err)
	_, err = sessions.Commit(
		t.Context(),
		sessionstore.Commit{
			SessionID: root,
			Accept: []sessionstore.Accept{
				{InputID: input.Input.ID, State: sessionstore.InputStateAccepted, Content: words, LinkRef: -1},
			},
		},
	)
	require.NoError(t, err)
	desk, err := store.LoadDesk(t.Context(), root, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, desk.Conversation, 3)
	assert.Equal(t, userKind, desk.Conversation[2].Kind)
	text := Render(View{Desk: desk}, now.Add(2*time.Hour))
	assert.True(t, strings.HasSuffix(text, "[…]\n"))
	assert.NotContains(t, text, strings.Repeat("я", dialogTextLimit))
	assert.Less(t, strings.Index(text, "spoken answer"), strings.LastIndex(text, "user: «"))
}

func seedDeskExchange(t *testing.T, db *sql.DB, sessions *sessionstore.Store, root int64, index int, at time.Time) {
	t.Helper()
	request := fmt.Sprintf("request %02d", index)
	input, err := sessions.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: request},
	)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE session_inbox SET received_at = ? WHERE id = ?`, at, input.Input.ID)
	require.NoError(t, err)
	kind := sessionstore.OutputMessagePersistent
	if index%2 == 1 {
		kind = sessionstore.OutputMessageReplaceable
	}
	_, err = sessions.Commit(t.Context(), sessionstore.Commit{
		SessionID: root,
		At:        at.Add(time.Second),
		Accept: []sessionstore.Accept{
			{InputID: input.Input.ID, State: sessionstore.InputStateAccepted, Content: request, LinkRef: -1},
		},
		Messages: []*transcript.Message{
			{
				Role:       llmwire.RoleAssistant,
				Content:    fmt.Sprintf("confirming done %02d", index),
				FinishType: llmwire.FinishStop,
			},
		},
		Outputs: []sessionstore.Output{
			{Type: kind, Content: fmt.Sprintf("spoken answer %02d", index), Phase: "final", MessageRef: 1},
		},
	})
	require.NoError(t, err)
}

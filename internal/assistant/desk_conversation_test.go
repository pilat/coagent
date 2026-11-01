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
				Type:       sessionstore.OutputMessage,
				Key:        "host:error",
				Content:    "host error notice",
				MessageRef: -1,
			},
			{
				Type:       sessionstore.OutputMessage,
				Key:        "host:compaction",
				Content:    "host compaction notice",
				MessageRef: -1,
			},
			{
				Type:       sessionstore.OutputMessage,
				Key:        "input:status",
				Content:    "host status receipt",
				MessageRef: -1,
			},
			{
				Type: sessionstore.OutputMessage, Key: "relay:1", Content: "work answer", MessageRef: -1,
				Attributes: map[string]any{sessionstore.RelayedSessionAttribute: int64(42)},
			},
			{Type: sessionstore.OutputMessage, Key: "forward:1:42", Content: "→ #42 work (forwarded)", MessageRef: -1},
			{Type: sessionstore.OutputMessage, Key: "tool:read:direct:0", Content: "host tool receipt", MessageRef: -1},
		}},
	)
	require.NoError(t, err)
	desk, err := store.LoadDesk(t.Context(), root, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 52, desk.Spoken)
	require.Len(t, desk.Conversation, 40)
	for i, line := range desk.Conversation[:38] {
		pair := 6 + i/2
		if i%2 == 0 {
			assert.Equal(t, userKind, line.Kind)
			assert.Equal(t, fmt.Sprintf("request %02d", pair), line.Text)
		} else {
			assert.Equal(t, sayKind, line.Kind)
			assert.Equal(t, fmt.Sprintf("spoken answer %02d", pair), line.Text)
		}
	}
	assert.Equal(t, relayKind, desk.Conversation[38].Kind)
	assert.Equal(t, forwardKind, desk.Conversation[39].Kind)
	text := Render(View{Desk: desk}, now.Add(time.Hour))
	assert.Contains(t, text, "last 40 of 52")
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

func TestAssistantDeskForwardAndRelayConversation(t *testing.T) {
	db, sessions, store, root := assistantFixture(t)
	now := time.Now().UTC()
	seedDeskExchange(t, db, sessions, root, 0, now)
	seedDeskUserLine(t, db, sessions, root, "forward this", now.Add(2*time.Second))
	for i, output := range []sessionstore.Output{
		{Type: sessionstore.OutputMessage, Key: "forward:10:42", Content: "→ #42 work (forwarded)", MessageRef: -1},
		{
			Type: sessionstore.OutputMessage, Key: "relay:11", Content: "first work answer", MessageRef: -1,
			Attributes: map[string]any{sessionstore.RelayedSessionAttribute: int64(42)},
		},
	} {
		_, err := sessions.Commit(
			t.Context(),
			sessionstore.Commit{
				SessionID: root,
				At:        now.Add(time.Duration(i+3) * time.Second),
				Outputs:   []sessionstore.Output{output},
			},
		)
		require.NoError(t, err)
	}
	seedDeskUserLine(t, db, sessions, root, "later request", now.Add(5*time.Second))
	_, err := sessions.Commit(
		t.Context(),
		sessionstore.Commit{SessionID: root, At: now.Add(6 * time.Second), Outputs: []sessionstore.Output{{
			Type: sessionstore.OutputMessage, Key: "relay:12", Content: "unrelated work answer", MessageRef: -1,
			Attributes: map[string]any{sessionstore.RelayedSessionAttribute: int64(42)},
		}}},
	)
	require.NoError(t, err)
	desk, err := store.LoadDesk(t.Context(), root, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, desk.Conversation, 7)
	assert.Equal(t, 7, desk.Spoken)
	var kinds []string
	for _, line := range desk.Conversation {
		kinds = append(kinds, line.Kind)
	}
	assert.Equal(t, []string{userKind, sayKind, userKind, forwardKind, relayKind, userKind, relayKind}, kinds)
	assert.Equal(t, int64(42), desk.Conversation[4].Origin)
	text := Render(View{Desk: desk}, now.Add(time.Minute))
	assert.Contains(t, text, "user: «forward this»")
	assert.Contains(t, text, "you: «→ #42 work (forwarded)»")
	assert.Contains(t, text, "#42: «first work answer»")
	assert.Contains(t, text, "#42: «unrelated work answer»")
	assert.Less(t, strings.Index(text, "user: «forward this»"), strings.Index(text, "you: «→ #42"))
	assert.Greater(t, strings.Index(text, "user: «later request»"), strings.LastIndex(text, "you: «"))
	assert.NotContains(t, text, "you: «unrelated work answer»")
}

func TestAssistantDeskRoutedLinesAreClipped(t *testing.T) {
	for _, kind := range []string{relayKind, forwardKind} {
		var b strings.Builder
		now := time.Now().UTC()
		renderConversation(&b, []Line{{Kind: kind, Text: strings.Repeat("界", 2500), Origin: 42, At: now}}, 1, now)
		assert.True(t, strings.HasSuffix(b.String(), "[…]\n"))
		assert.NotContains(t, b.String(), strings.Repeat("界", dialogTextLimit))
	}
}

func seedDeskUserLine(t *testing.T, db *sql.DB, sessions *sessionstore.Store, root int64, text string, at time.Time) {
	t.Helper()
	input, err := sessions.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: text},
	)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE session_inbox SET received_at = ? WHERE id = ?`, at, input.Input.ID)
	require.NoError(t, err)
	_, err = sessions.Commit(t.Context(), sessionstore.Commit{
		SessionID: root,
		Accept: []sessionstore.Accept{
			{InputID: input.Input.ID, State: sessionstore.InputStateAccepted, Content: text, LinkRef: -1},
		},
	})
	require.NoError(t, err)
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
	kind := sessionstore.OutputMessage
	if index%2 == 1 {
		kind = sessionstore.OutputMessage
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

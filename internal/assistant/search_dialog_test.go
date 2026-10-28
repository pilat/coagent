package assistant

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAssistantSearchOwnDialogAndAround(t *testing.T) {
	t.Parallel()
	db, sessions, store, root := assistantFixture(t)
	first, reply := answered(t, sessions, root, "own dialogue request", "own dialogue reply")
	_, err := sessions.Commit(t.Context(), sessionstore.Commit{SessionID: root, Messages: []*transcript.Message{
		{
			Role:       llmwire.RoleAssistant,
			Content:    "own dialogue speech with tools",
			ToolCalls:  []byte(`[{"id":"look","name":"projects","arguments":{}}]`),
			FinishType: llmwire.FinishToolCalls,
		},
		{Role: llmwire.RoleTool, ToolCallID: "look", ToolName: "projects", Content: "own dialogue tool noise"},
		{Role: llmwire.RoleUser, Content: "own dialogue host head"},
		{
			Role:           llmwire.RoleAssistant,
			Content:        "own dialogue rejected speech",
			FinishType:     llmwire.FinishLength,
			RejectedReason: "output_length",
		},
	}})
	require.NoError(t, err)
	answered(t, sessions, root, "own dialogue newest request", "done")
	_, err = db.ExecContext(
		t.Context(),
		`UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE session_id = ?`,
		root,
	)
	require.NoError(t, err)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	_, final := answered(t, sessions, work, "work request", "own dialogue work final")
	foreign := workSession(t, sessions, map[string]any{"manager_id": "foreign"})
	_, err = db.ExecContext(t.Context(), `UPDATE sessions SET agent_type = 'assistant' WHERE id = ?`, foreign)
	require.NoError(t, err)
	_, foreignReply := answered(t, sessions, foreign, "own dialogue foreign request", "own dialogue foreign reply")
	scope := Scope{Root: root, Owner: testOwner}
	hits, err := store.Search(t.Context(), scope, "dialogue")
	require.NoError(t, err)
	assert.Contains(t, hitRefs(hits), "msg "+itoa(first))
	assert.Contains(t, hitRefs(hits), "msg "+itoa(reply))
	assert.Contains(t, hitRefs(hits), "msg "+itoa(final))
	assert.NotContains(t, hitRefs(hits), "msg "+itoa(foreignReply))
	toolSpeechFound := false
	for _, hit := range hits {
		toolSpeechFound = toolSpeechFound || strings.Contains(hit.Snippet, "speech with tools")
		assert.NotContains(t, hit.Snippet, "tool noise")
		assert.NotContains(t, hit.Snippet, "host head")
		assert.NotContains(t, hit.Snippet, "rejected")
	}
	assert.True(t, toolSpeechFound, "own assistant speech remains searchable when it carries tool calls")
	text, err := store.Around(t.Context(), scope, "msg "+itoa(reply), 2)
	require.NoError(t, err)
	assert.Contains(t, text, "own dialogue request")
	assert.Contains(t, text, "own dialogue reply")
	assert.Contains(t, text, "speech with tools")
	assert.Contains(t, text, "newest request")
	assert.NotContains(t, text, "tool noise")
	_, err = store.Around(t.Context(), scope, "msg "+itoa(foreignReply), 2)
	require.ErrorContains(t, err, "not a message of a session you can see")
	_, err = store.Around(t.Context(), scope, "line 1", 1)
	require.ErrorContains(t, err, "use \"msg <id>\"")
}

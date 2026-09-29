package sessionstore

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
	"github.com/pilat/coagent/migrations"
)

func TestToolResultCallRef_LegacyReplayAndRepeatedProviderID(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)
	owner, err := s.InsertMessage(ctx, sessionID, &transcript.Message{
		Role: "assistant", ToolCalls: []byte(`[{"ID":"reused","Name":"read"}]`),
	})
	require.NoError(t, err)
	legacy, err := s.InsertMessage(ctx, sessionID, toolResultRow("reused", "read", "first", false))
	require.NoError(t, err)
	entry := ToolResultEntry{CallRef: CallRef{AssistantMessageID: owner}, Message: toolResultRow("reused", "read", "first", false)}
	ids, _, err := s.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
	require.NoError(t, err)
	require.Equal(t, legacy, ids[0])
	var unchanged sql.NullInt64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT tool_call_owner_id FROM messages WHERE id = ?`, legacy).Scan(&unchanged))
	require.False(t, unchanged.Valid)

	nextOwner, err := s.InsertMessage(ctx, sessionID, &transcript.Message{
		Role: "assistant", ToolCalls: []byte(`[{"ID":"reused","Name":"read"}]`),
	})
	require.NoError(t, err)
	entry.CallRef.AssistantMessageID = nextOwner
	entry.Message.Content = "second"
	ids, _, err = s.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
	require.NoError(t, err)
	require.NotEqual(t, legacy, ids[0])
	replayed, _, err := s.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
	require.NoError(t, err)
	require.Equal(t, ids, replayed)
	entry.CallRef.Index = 1
	_, _, err = s.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
	require.Error(t, err)
}

func TestToolResultCallRef_RejectsAmbiguousLegacyReplay(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)
	owner, err := s.InsertMessage(ctx, sessionID, &transcript.Message{
		Role: "assistant", ToolCalls: []byte(`[{"id":"same","name":"read"},{"id":"same","name":"read"}]`),
	})
	require.NoError(t, err)
	_, err = s.InsertMessage(ctx, sessionID, toolResultRow("same", "read", "old", false))
	require.NoError(t, err)
	_, _, err = s.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{{
		CallRef: CallRef{AssistantMessageID: owner}, Message: toolResultRow("same", "read", "old", false),
	}})
	require.ErrorContains(t, err, "ambiguous legacy")
}

func TestToolResultCallRef_MigrationPreservesLegacyRows(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)
	legacy, err := s.InsertMessage(ctx, sessionID, toolResultRow("legacy", "read", "original", false))
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DROP INDEX messages_tool_call_identity`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `ALTER TABLE messages DROP COLUMN tool_call_index`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `ALTER TABLE messages DROP COLUMN tool_call_owner_id`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("00046_tool_result_call_identity.sql")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, string(raw))
	require.NoError(t, err)
	var content string
	var owner sql.NullInt64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT content, tool_call_owner_id FROM messages WHERE id = ?`, legacy).Scan(&content, &owner))
	require.Equal(t, "original", content)
	require.False(t, owner.Valid)
}

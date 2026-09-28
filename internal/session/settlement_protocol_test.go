package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestOpenTranscriptWithoutOutputPreservesSettlementProtocol(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	ms := newMessageStore(store, sessionID, nil)
	call := PendingToolCall{ID: "external-call", Name: "task"}
	require.NoError(t, ms.addAssistantMessage(t.Context(), &llmwire.Response{
		ToolCalls: []llmwire.ToolCall{{ID: call.ID, Name: call.Name}},
	}))
	candidateID := seedSettlementCandidate(t, store, sessionID)
	staged := map[string]string{call.ID: call.Name}
	stale, err := OpenTranscript(t.Context(), store, nil, sessionID, staged)
	require.NoError(t, err)
	current, err := OpenTranscript(t.Context(), store, nil, sessionID, staged)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), "CREATE TRIGGER reject_settlement BEFORE INSERT ON messages "+
		"WHEN NEW.role = 'tool' BEGIN SELECT RAISE(ABORT, 'settlement rejected'); END")
	require.NoError(t, err)
	_, err = current.ResolvePendingCall(t.Context(), call, "completed")
	require.ErrorContains(t, err, "settlement rejected")
	assertSettlementCandidate(t, store, sessionID, candidateID)
	assert.Equal(t, []PendingToolCall{call}, current.PendingExternalCalls())
	_, err = db.ExecContext(t.Context(), "DROP TRIGGER reject_settlement")
	require.NoError(t, err)

	_, err = current.ResolvePendingCall(t.Context(), call, "completed")
	require.NoError(t, err)
	state, err := store.LoadCompletionCheckState(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID)
	newerID := seedSettlementCandidate(t, store, sessionID)
	_, err = stale.ResolvePendingCall(t.Context(), call, "completed")
	require.NoError(t, err)
	assertSettlementCandidate(t, store, sessionID, newerID)
	require.NoError(t, stale.ReloadDeliveredCompletion(t.Context()))
	assert.Empty(t, stale.PendingExternalCalls())
	var results, outputs int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM messages WHERE role = 'tool' AND tool_call_id = ?", call.ID).Scan(&results))
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM session_outbox").Scan(&outputs))
	assert.Equal(t, 1, results)
	assert.Zero(t, outputs)
}

func TestToolResultsWithoutOutputPreserveAtomicAttachmentIdentity(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			db, store, sessionID := newFinalOutputStore(t)
			ms := newMessageStore(store, sessionID, nil)
			count := 1
			if batch {
				count = 2
			}
			calls := make([]llmwire.ToolCall, count)
			commits := make([]toolResultCommit, count)
			for i := range count {
				callID := fmt.Sprintf("image-%d", i)
				calls[i] = llmwire.ToolCall{ID: callID, Name: "read"}
				commits[i] = toolResultCommit{
					message: llmwire.Message{
						Role: llmwire.RoleTool, ToolCallID: callID, ToolName: "read",
						Content: "partial image", ToolError: true, Images: demoRefs,
					},
					direct: []string{"suppressed output"},
				}
			}
			require.NoError(t, ms.addAssistantMessage(t.Context(), &llmwire.Response{ToolCalls: calls}))
			candidateID := seedSettlementCandidate(t, store, sessionID)
			write := func(values []toolResultCommit) error {
				if batch {
					return ms.commitToolResults(t.Context(), values)
				}
				m := values[0].message
				return ms.addToolResultOutputTyped(t.Context(), m.ToolCallID, m.ToolName,
					m.Content, m.Images, values[0].direct, m.ToolError)
			}
			_, err := db.ExecContext(t.Context(), fmt.Sprintf(
				"CREATE TRIGGER reject_last_image BEFORE INSERT ON messages WHEN NEW.tool_call_id = 'image-%d' "+
					"BEGIN SELECT RAISE(ABORT, 'image result rejected'); END", count-1))
			require.NoError(t, err)
			require.ErrorContains(t, write(commits), "image result rejected")
			assertSettlementCandidate(t, store, sessionID, candidateID)
			var resultCount int
			require.NoError(t, db.QueryRowContext(t.Context(),
				"SELECT COUNT(*) FROM messages WHERE role = 'tool'").Scan(&resultCount))
			assert.Zero(t, resultCount, "a later batch failure rolls back earlier results and invalidation")
			_, err = db.ExecContext(t.Context(), "DROP TRIGGER reject_last_image")
			require.NoError(t, err)
			require.NoError(t, write(commits))
			state, err := store.LoadCompletionCheckState(t.Context(), sessionID)
			require.NoError(t, err)
			assert.Nil(t, state.CandidateID)
			newerID := seedSettlementCandidate(t, store, sessionID)
			require.NoError(t, ms.reloadMessages(t.Context()))
			before := ms.getMessages()
			beforeIDs := ms.getRowIDs()
			require.NoError(t, write(commits))
			assert.Equal(t, before, ms.getMessages())
			assert.Equal(t, beforeIDs, ms.getRowIDs())
			assertSettlementCandidate(t, store, sessionID, newerID)
			conflicting := append([]toolResultCommit(nil), commits...)
			conflicting[0].message.Images = append([]llmwire.ImageRef(nil), demoRefs...)
			conflicting[0].message.Images[0].Size++
			require.ErrorIs(t, write(conflicting), sessionstore.ErrOutputConflict)
			assertSettlementCandidate(t, store, sessionID, newerID)

			reloaded := newMessageStore(store, sessionID, nil)
			require.NoError(t, reloaded.reloadMessages(t.Context()))
			results := 0
			for _, message := range reloaded.getMessages() {
				if message.Role == llmwire.RoleTool {
					results++
					assert.Equal(t, demoRefs, message.Images)
					assert.True(t, message.ToolError)
				}
			}
			assert.Equal(t, count, results)
			var outputs int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM session_outbox").Scan(&outputs))
			assert.Zero(t, outputs)
		})
	}
}

func seedSettlementCandidate(t *testing.T, store sessionstore.ResponseDispositionStore, sessionID int64) int64 {
	t.Helper()
	result, err := store.CommitAcceptedResponseDisposition(
		context.Background(),
		sessionstore.AcceptedResponseDisposition{
			SessionID: sessionID, RootID: sessionID, Kind: sessionstore.ResponseDispositionCandidate,
			Message: &transcript.Message{Role: llmwire.RoleAssistant, Content: "candidate"},
			Nudge:   &transcript.Message{Role: llmwire.RoleUser, Content: "confirm completion"},
		},
	)
	require.NoError(t, err)
	return result.MessageID
}

func assertSettlementCandidate(
	t *testing.T,
	store sessionstore.ResponseDispositionStore,
	sessionID, candidateID int64,
) {
	t.Helper()
	state, err := store.LoadCompletionCheckState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, candidateID, *state.CandidateID)
}

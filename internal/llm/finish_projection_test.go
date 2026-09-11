package llm

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestRejectedAttemptProjectionNeverReachesProviderConverters(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "finish-projection.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))

	_, err = db.ExecContext(t.Context(), `INSERT INTO projects (id, work_dir, name) VALUES (1, ?, 'p')`, t.TempDir())
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	session, err := store.CreateSession(t.Context(), 1, "model", "", nil)
	require.NoError(t, err)
	_, err = store.InsertMessage(t.Context(), session.ID, &transcript.Message{
		Role: llmwire.RoleUser, Content: "task",
	})
	require.NoError(t, err)
	_, err = store.CommitRejectedResponse(t.Context(), sessionstore.RejectedResponse{
		SessionID: session.ID, RootID: session.ID, Iteration: 1,
		Message: &transcript.Message{
			Role: llmwire.RoleAssistant, Content: "rejected provider text",
			FinishType: llmwire.FinishLength, RejectedReason: sessionstore.RejectedReasonOutputLength,
		},
	})
	require.NoError(t, err)

	stored, err := store.LoadActiveMessages(t.Context(), session.ID)
	require.NoError(t, err)

	messages := make([]llmwire.Message, len(stored))
	for i, message := range stored {
		messages[i] = llmwire.Message{Role: message.Role, Content: message.Content}
	}

	openAIJSON, err := json.Marshal((&openaiClient{}).convertMessages(messages))
	require.NoError(t, err)
	anthropicJSON, err := json.Marshal((&anthropicClient{}).convertMessages(messages))
	require.NoError(t, err)
	for _, payload := range [][]byte{openAIJSON, anthropicJSON} {
		assert.NotContains(t, string(payload), "rejected provider text")
		assert.Contains(t, string(payload), sessionstore.OutputLengthRecoveryPrompt)
	}
}

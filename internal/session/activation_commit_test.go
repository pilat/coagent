package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type toolResultFailureStore struct {
	Store
	err error
}

func (s *toolResultFailureStore) Commit(
	ctx context.Context,
	c sessionstore.Commit,
) (*sessionstore.CommitResult, error) {
	if len(c.ToolResults) > 0 {
		return nil, s.err
	}

	return s.Store.Commit(ctx, c)
}

func TestToolStep_SuccessfulMutationSpendsGrantBeforeResultCommit(t *testing.T) {
	for _, commitErr := range []error{errors.New("result commit unavailable"), sessionstore.ErrSessionStopping} {
		t.Run(commitErr.Error(), func(t *testing.T) {
			db, store, s := newBoundaryFixture(t, nil)
			s.registry.Register(budget.NewTool(store, s.id, true))
			require.NoError(t, s.prepareRun(t.Context()))
			_, err := store.Enqueue(t.Context(), sessionstore.Input{
				SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "/budget one minute",
			})
			require.NoError(t, err)
			_, err = s.boundaryStep(t.Context(), &runState{})
			require.NoError(t, err)
			require.NotNil(t, s.currentActivation)
			calls := []llmwire.ToolCall{{
				ID: "budget-first", Name: budget.ToolID, Arguments: []byte(`{"action":"set","duration":"1m"}`),
			}}
			require.NoError(t, appendTestAssistant(t.Context(), s.ms, &llmwire.Response{ToolCalls: calls}))
			s.store = &toolResultFailureStore{Store: store, err: commitErr}
			runErr := s.toolStep(t.Context(), calls)
			require.ErrorIs(t, runErr, commitErr)
			assert.Nil(t, s.currentActivation, "a successful mutation cannot authorize a second call")
			record, err := store.Get(t.Context(), s.id)
			require.NoError(t, err)
			require.NotNil(t, record.DurationSeconds)
			assert.Equal(t, int64(60), *record.DurationSeconds)
			_, err = s.finishRun(t.Context(), &runState{}, runErr)
			require.ErrorIs(t, err, commitErr)
			outputs, err := store.ChatOutputs(t.Context(), s.id, 0, 100)
			require.NoError(t, err)
			for _, output := range outputs {
				assert.NotContains(t, output.Content, "was not changed")
			}
			var receipts int
			require.NoError(t, db.QueryRowContext(
				t.Context(),
				"SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key = 'tool:budget-first:direct:0'",
				s.id,
			).Scan(&receipts))
			assert.Equal(t, 1, receipts)
			s.store = store
			calls[0].ID = "budget-second"
			calls[0].Arguments = []byte(`{"action":"set","duration":"2m"}`)
			require.NoError(t, appendTestAssistant(t.Context(), s.ms, &llmwire.Response{ToolCalls: calls}))
			require.NoError(t, s.toolStep(t.Context(), calls))
			messages := s.ms.getMessages()
			require.NotEmpty(t, messages)
			assert.Contains(t, messages[len(messages)-1].Content,
				"This change requires a current user message beginning with /budget.")
			record, err = store.Get(t.Context(), s.id)
			require.NoError(t, err)
			assert.Equal(t, int64(1), record.Generation)
			assert.Equal(t, int64(60), *record.DurationSeconds)
		})
	}
}

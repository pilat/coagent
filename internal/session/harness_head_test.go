package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type headConflictStore struct {
	*sessionstore.Store
	conflicted bool
}

func TestHeadRunKeepsToolsAndMidRunInput(t *testing.T) {
	_, store, s := newHeadFixture(t, nil)
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "A"},
	)
	require.NoError(t, err)
	tool := &stubTool{id: "read", result: "search result retained"}
	s.registry.Register(tool)
	renders := 0
	s.head = func(context.Context) (string, error) { renders++; return "fresh desk", nil }
	client := &loopScriptLLM{onCall: func(n int, messages []llmwire.Message) (*llmwire.Response, error) {
		if n == 1 {
			_, enqueueErr := store.Enqueue(
				t.Context(),
				sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "B"},
			)
			require.NoError(t, enqueueErr)
			return &llmwire.Response{
				FinishType: llmwire.FinishToolCalls,
				ToolCalls:  []llmwire.ToolCall{call("read-1", "read")},
			}, nil
		}
		if n == 2 {
			require.Len(t, messages, 5)
			assert.Equal(t, "fresh desk", messages[0].Content)
			assert.Contains(t, messages[1].Content, "A")
			assert.Equal(t, llmwire.RoleAssistant, messages[2].Role)
			assert.Equal(t, "search result retained", messages[3].Content)
			assert.Contains(t, messages[4].Content, "B")
		}
		return textResponse("done"), nil
	}}
	s.llmClient = client
	_, err = s.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, renders)
}

func TestHeadBoundaryConflictRollsBackAndRetries(t *testing.T) {
	db, backing, s := newHeadFixture(t, []llmwire.Message{usr("old desk"), usr("A")})
	store := &headConflictStore{Store: backing}
	s.store, s.ms.store = store, store
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "superseded"},
	)
	require.NoError(t, err)
	renders := 0
	s.head = func(context.Context) (string, error) { renders++; return "fresh desk", nil }
	run := &runState{}
	accepted, err := s.boundaryStep(t.Context(), run)
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.True(t, run.headCommitted)
	assert.Equal(t, 2, renders)
	rows, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "fresh desk", rows[0].Content)
	assert.Contains(t, rows[1].Content, "replacement")
	var heads int
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE session_id = ? AND content = 'fresh desk'`, s.id).
			Scan(&heads),
	)
	assert.Equal(t, 1, heads, "the failed batch leaves no head row")
}

func TestHeadBoundaryReplayDoesNotAppendOrCompactAgain(t *testing.T) {
	_, store, s := newHeadFixture(t, []llmwire.Message{usr("old desk"), asst("old answer")})
	input, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "B"},
	)
	require.NoError(t, err)
	batch := s.newBoundaryBatch()
	_, err = s.acceptBoundaryText(t.Context(), &runState{}, batch, input.Input, "")
	require.NoError(t, err)
	_, err = s.commitBoundaryBatch(t.Context(), &runState{}, batch, 1)
	require.NoError(t, err)
	_, err = store.Commit(
		t.Context(),
		sessionstore.Commit{
			SessionID: s.id,
			Messages:  []*transcript.Message{{Role: llmwire.RoleAssistant, Content: "later answer"}},
		},
	)
	require.NoError(t, err)
	before, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	_, err = store.Commit(t.Context(), batch.commit)
	require.NoError(t, err)
	after, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func (s *headConflictStore) Commit(ctx context.Context, c sessionstore.Commit) (*sessionstore.CommitResult, error) {
	if c.State.ResetContext && !s.conflicted {
		s.conflicted = true
		if err := s.WithTx(ctx, func(tx *sql.Tx) error {
			return sessionstore.HandleAssistantInputTx(
				ctx,
				tx,
				c.SessionID,
				c.Accept[0].InputID,
				"host won",
				time.Now().UTC(),
			)
		}); err != nil {
			return nil, err
		}
		if _, err := s.Enqueue(
			ctx,
			sessionstore.Input{SessionID: c.SessionID, Source: sessionstore.InputSourceUser, Content: "replacement"},
		); err != nil {
			return nil, err
		}
	}
	return s.Store.Commit(ctx, c)
}

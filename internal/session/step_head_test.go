package session

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestHeadBoundaryReplacesTranscript(t *testing.T) {
	for _, previous := range [][]llmwire.Message{
		nil,
		{usr("old desk"), usr("A")},
		{usr("old desk"), usr("A"), asst("working", call("old-read", "read")), toolRes("old-read"), asst("answer")},
	} {
		t.Run(testHeadHistoryName(previous), func(t *testing.T) {
			db, store, s := newHeadFixture(t, previous)
			ctx := t.Context()
			_, err := store.Commit(ctx, sessionstore.Commit{SessionID: s.id, State: sessionstore.StatePatch{
				ContextBaseline: &sessionstore.ContextBaseline{
					Model:        "model",
					PromptTokens: 500,
					MessageCount: len(previous),
				},
			}})
			require.NoError(t, err)
			s.installPersistedBaseline(
				&sessionstore.ContextBaseline{Model: "model", PromptTokens: 500, MessageCount: len(previous)},
			)
			s.prompt.Todos.Add("old todo", todo.PriorityMedium)
			s.loopDetector.record([]toolRecord{{name: "old"}})
			_, err = db.ExecContext(ctx, `UPDATE sessions SET empty_stop_streak = 3,
completion_check_candidate_id = (SELECT MAX(id) FROM messages WHERE session_id = ?) WHERE id = ?`, s.id, s.id)
			require.NoError(t, err)
			input, err := store.Enqueue(
				ctx,
				sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "B"},
			)
			require.NoError(t, err)
			run := &runState{}
			accepted, err := s.boundaryStep(ctx, run)
			require.NoError(t, err)
			assert.True(t, accepted)
			assert.True(t, run.headCommitted)
			rows, err := store.LoadActiveMessages(ctx, s.id)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			assert.Equal(t, llmwire.RoleUser, rows[0].Role)
			assert.Equal(t, "fresh desk", rows[0].Content)
			assert.Equal(t, llmwire.RoleUser, rows[1].Role)
			assert.Contains(t, rows[1].Content, "B")
			var linked int64
			require.NoError(
				t,
				db.QueryRowContext(ctx, `SELECT accepted_message_id FROM session_inbox WHERE id = ?`, input.Input.ID).
					Scan(&linked),
			)
			assert.Equal(t, rows[1].ID, linked)
			var compacted int
			require.NoError(
				t,
				db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ? AND compacted_at IS NOT NULL`, s.id).
					Scan(&compacted),
			)
			assert.Equal(t, len(previous), compacted)
			record, err := store.GetSession(ctx, s.id)
			require.NoError(t, err)
			assert.Nil(t, record.ContextBaseline())
			assert.Nil(t, record.CompletionCheckCandidateID)
			assert.Zero(t, record.EmptyStopStreak)
			assert.Nil(t, s.loadContextBaseline())
			assert.Zero(t, s.prompt.Todos.Count())
			assert.Empty(t, s.loopDetector.window)
			assert.Equal(t, 1, compactionHeaderSize(s.ms.getMessages()))
		})
	}
}

func TestHeadBoundaryGates(t *testing.T) {
	for _, tc := range []struct {
		name              string
		previous          []llmwire.Message
		input             string
		nilHead, external bool
		accepted          bool
	}{
		{name: "pending in-loop call", previous: []llmwire.Message{usr("A"), asst("", call("read-1", "read"))}, input: "B"},
		{name: "pending external call", previous: []llmwire.Message{usr("A"), asst("", call("owned", tool.IDConfigEdit))}, input: "B", external: true},
		{name: "blocked kill", input: "/kill"},
		{name: "no new input", previous: []llmwire.Message{usr("old desk"), usr("A")}},
		{name: "ordinary nil hook", previous: []llmwire.Message{usr("A"), asst("answer")}, input: "B", nilHead: true, accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, s := newHeadFixture(t, tc.previous)
			calls := 0
			s.head = func(context.Context) (string, error) { calls++; return "unexpected desk", nil }
			if tc.nilHead {
				s.head = nil
			}
			if tc.external {
				s.stagedCalls = map[string]string{"owned": tool.IDConfigEdit}
			}
			if tc.input != "" {
				_, err := store.Enqueue(
					t.Context(),
					sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: tc.input},
				)
				require.NoError(t, err)
			}
			accepted, err := s.boundaryStep(t.Context(), &runState{})
			require.NoError(t, err)
			assert.Equal(t, tc.accepted, accepted)
			assert.Zero(t, calls)
			rows, err := store.LoadActiveMessages(t.Context(), s.id)
			require.NoError(t, err)
			count := len(tc.previous)
			if tc.accepted {
				count++
			}
			assert.Len(t, rows, count)
			for i, previous := range tc.previous {
				assert.Equal(t, previous.Content, rows[i].Content)
			}
		})
	}
}

func TestHeadResumeWithoutNewInput(t *testing.T) {
	_, _, s := newHeadFixture(t, []llmwire.Message{usr("old desk"), usr("A")})
	s.head = func(context.Context) (string, error) { t.Error("resume must keep its head"); return "", nil }
	client := &loopScriptLLM{onCall: func(n int, messages []llmwire.Message) (*llmwire.Response, error) {
		if n == 1 {
			require.Len(t, messages, 2)
			assert.Equal(t, "old desk", messages[0].Content)
			assert.Equal(t, "A", messages[1].Content)
		}
		return textResponse("done"), nil
	}}
	s.llmClient = client
	_, err := s.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, client.calls)
}

func TestHeadBlockedKillDoesNotCallModel(t *testing.T) {
	_, store, s := newHeadFixture(t, nil)
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "/kill"},
	)
	require.NoError(t, err)
	client := &loopScriptLLM{}
	s.llmClient = client
	_, err = s.Run(t.Context())
	require.NoError(t, err)
	assert.Zero(t, client.calls)
	rows, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestHeadRenderErrorIsDurable(t *testing.T) {
	db, store, s := newHeadFixture(t, nil)
	cause := errors.New("desk storage unavailable")
	s.head = func(context.Context) (string, error) { return "", cause }
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "B"},
	)
	require.NoError(t, err)
	result, err := s.Run(t.Context())
	require.ErrorIs(t, err, cause)
	assert.Contains(t, result.ErrorNotice, "desk storage unavailable")
	record, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusError, record.Status)
	var notice string
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), `SELECT content FROM session_outbox WHERE session_id = ? AND source_key LIKE 'run:%:error'`, s.id).
			Scan(&notice),
	)
	assert.Equal(t, result.ErrorNotice, notice)
	rows, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	assert.Empty(t, rows)
	pending, err := store.ListPending(t.Context(), s.id)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func newHeadFixture(t *testing.T, previous []llmwire.Message) (*sql.DB, *sessionstore.Store, *Session) {
	t.Helper()
	db, store, id := newAttachmentsStore(t)
	var rows []*transcript.Message
	for i := range previous {
		row, err := storedMessage(&previous[i])
		require.NoError(t, err)
		rows = append(rows, row)
	}
	_, err := store.Commit(t.Context(), sessionstore.Commit{SessionID: id, Messages: rows})
	require.NoError(t, err)
	s := headFixtureSession(t, store, id)
	return db, store, s
}

func headFixtureSession(t *testing.T, store Store, id int64) *Session {
	t.Helper()
	backing, ok := store.(interface {
		GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	})
	require.True(t, ok)
	record, err := backing.GetSession(t.Context(), id)
	require.NoError(t, err)
	prompt := sessionprompt.NewBuilder("stable", "")
	prompt.Todos = todo.New()
	s, err := New(t.Context(), Input{
		Record:        record,
		Client:        &loopScriptLLM{responses: []*llmwire.Response{textResponse("done"), textResponse("confirmed")}},
		Loader:        loader.New(),
		Registry:      tool.NewRegistry(),
		Prompt:        prompt,
		Store:         store,
		Head:          func(context.Context) (string, error) { return "fresh desk", nil },
		OutputEnabled: true,
	})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func testHeadHistoryName(previous []llmwire.Message) string {
	switch len(previous) {
	case 0:
		return "empty transcript"
	case 2:
		return "unanswered previous input"
	default:
		return "completed activation with tools"
	}
}

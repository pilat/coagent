package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*answerInputTool)(nil)

type answerInputTool struct {
	result  tool.Result
	execute func()
}

func TestSilentFinishInputObligations(t *testing.T) {
	for _, tc := range []struct {
		name                                                   string
		source                                                 sessionstore.InputSource
		silent, answers, failed, lateHuman, resume, wantSilent bool
	}{
		{name: "event", source: sessionstore.InputSourceAgent, silent: true, wantSilent: true},
		{name: "human", source: sessionstore.InputSourceUser, silent: true},
		{name: "human answered by tool", source: sessionstore.InputSourceUser, silent: true, answers: true, wantSilent: true},
		{name: "failed tool cannot answer", source: sessionstore.InputSourceUser, silent: true, answers: true, failed: true},
		{name: "later human renews obligation", source: sessionstore.InputSourceUser, silent: true, answers: true, lateHuman: true},
		{name: "work event", source: sessionstore.InputSourceAgent},
		{name: "work human", source: sessionstore.InputSourceUser},
		{name: "work tool answer", source: sessionstore.InputSourceUser, answers: true},
		{name: "work later human", source: sessionstore.InputSourceUser, answers: true, lateHuman: true},
		{name: "resume without input", silent: true, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, s := newSilentFinishFixture(t, tc.silent)
			if tc.resume {
				require.NoError(t, appendTestUser(t.Context(), s.ms, "unfinished previous turn"))
			} else {
				_, err := store.Enqueue(
					t.Context(),
					sessionstore.Input{SessionID: s.id, Source: tc.source, Content: "input"},
				)
				require.NoError(t, err)
			}
			emptyStep := 1
			if tc.answers {
				emptyStep++
				answer := &answerInputTool{
					result: tool.Result{Output: "answered", AnswersInput: true, IsError: tc.failed},
				}
				if tc.lateHuman {
					answer.execute = func() {
						_, err := store.Enqueue(
							t.Context(),
							sessionstore.Input{
								SessionID: s.id,
								Source:    sessionstore.InputSourceUser,
								Content:   "another question",
							},
						)
						require.NoError(t, err)
					}
				}
				s.registry.Register(answer)
			}
			nudged := false
			client := &loopScriptLLM{onCall: func(n int, messages []llmwire.Message) (*llmwire.Response, error) {
				if n < emptyStep {
					return toolCallResponse("answer-1", "answer_input"), nil
				}
				if n == emptyStep {
					return textResponse(" \n"), nil
				}
				if n == emptyStep+1 {
					state, err := store.LoadCompletionCheckState(t.Context(), s.id)
					require.NoError(t, err)
					assert.Equal(t, 1, state.EmptyStopStreak)
					assert.Contains(t, transcriptText(messages), "You returned an empty response")
					nudged = true
				}
				return textResponse("completed"), nil
			}}
			s.llmClient = client
			result, err := s.Run(t.Context())
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), s.id)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
			assert.Equal(t, !tc.wantSilent, nudged)
			var outputs int
			require.NoError(
				t,
				db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, s.id).
					Scan(&outputs),
			)
			if tc.wantSilent {
				assert.Equal(t, emptyStep, client.calls)
				assert.Empty(t, result.Final)
				assert.Zero(t, outputs)
				assert.Zero(t, record.EmptyStopStreak)
				assert.NotContains(t, transcriptText(toWireMessages(t, store, s.id)), "empty response")
			} else {
				assert.Equal(t, emptyStep+2, client.calls)
				assert.Equal(t, "completed", result.Final)
				assert.Equal(t, 1, outputs)
			}
		})
	}
}

func TestSilentFinishConfirmsCandidate(t *testing.T) {
	db, store, s := newSilentFinishFixture(t, true)
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceAgent, Content: "event"},
	)
	require.NoError(t, err)
	s.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("candidate answer"), textResponse("")}}
	result, err := s.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "candidate answer", result.Final)
	var content string
	var outputs int
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), `SELECT COUNT(*),MAX(content) FROM session_outbox WHERE session_id = ?`, s.id).
			Scan(&outputs, &content),
	)
	assert.Equal(t, 1, outputs)
	assert.Equal(t, "candidate answer", content)
	record, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	assert.Nil(t, record.CompletionCheckCandidateID)
	assert.NotNil(t, record.CompletionCheckConfirmedAnswerID)
	assert.Zero(t, record.EmptyStopStreak)
}

func TestSilentFinishDisabledKeepsCandidateEmptyNudge(t *testing.T) {
	_, store, s := newSilentFinishFixture(t, false)
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceAgent, Content: "event"},
	)
	require.NoError(t, err)
	client := &loopScriptLLM{onCall: func(n int, messages []llmwire.Message) (*llmwire.Response, error) {
		switch n {
		case 1:
			return textResponse("candidate"), nil
		case 2:
			return textResponse(""), nil
		default:
			state, err := store.LoadCompletionCheckState(t.Context(), s.id)
			require.NoError(t, err)
			assert.Equal(t, 1, state.EmptyStopStreak)
			assert.Contains(t, transcriptText(messages), "You returned an empty response")
			return textResponse("confirmed"), nil
		}
	}}
	s.llmClient = client
	result, err := s.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, client.calls)
	assert.Equal(t, "candidate", result.Final)
}

func TestAnswersInputRequiresCommittedToolResult(t *testing.T) {
	db, store, s := newSilentFinishFixture(t, true)
	_, err := store.Enqueue(
		t.Context(),
		sessionstore.Input{SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "question"},
	)
	require.NoError(t, err)
	r := &runState{answerOwed: true}
	_, err = s.boundaryStep(t.Context(), r)
	require.NoError(t, err)
	s.registry.Register(&answerInputTool{result: tool.Result{Output: "answered", AnswersInput: true}})
	response := toolCallResponse("answer-1", "answer_input")
	require.NoError(t, appendTestAssistant(t.Context(), s.ms, response))
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_result BEFORE INSERT ON messages
		WHEN NEW.role = 'tool' BEGIN SELECT RAISE(ABORT, 'result commit failed'); END`)
	require.NoError(t, err)
	err = s.toolStep(t.Context(), r, response.ToolCalls)
	require.ErrorContains(t, err, "result commit failed")
	assert.True(t, r.answerOwed)
}

func (t *answerInputTool) ID() string                  { return "answer_input" }
func (t *answerInputTool) Description() string         { return "answer the input" }
func (t *answerInputTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *answerInputTool) ParallelSafe() bool          { return false }
func (t *answerInputTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	if t.execute != nil {
		t.execute()
	}
	return &t.result, nil
}

func newSilentFinishFixture(t *testing.T, silent bool) (*sql.DB, *sessionstore.Store, *Session) {
	t.Helper()
	db, store, id := newAttachmentsStore(t)
	record, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	prompt := sessionprompt.NewBuilder("stable system", "")
	prompt.Todos = todo.New()
	s, err := New(t.Context(), Input{
		Record: record, Client: &loopScriptLLM{}, Store: store,
		Registry: tool.NewRegistry(), Loader: loader.New(), Prompt: prompt, OutputEnabled: true, SilentFinish: silent,
	})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return db, store, s
}

func toWireMessages(t *testing.T, store *sessionstore.Store, id int64) []llmwire.Message {
	t.Helper()
	rows, err := store.LoadActiveMessages(t.Context(), id)
	require.NoError(t, err)
	messages := make([]llmwire.Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, llmwire.Message{Role: row.Role, Content: row.Content})
	}
	return messages
}

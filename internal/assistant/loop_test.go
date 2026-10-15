package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

type loopFixture struct {
	t        *testing.T
	db       *sql.DB
	store    *sessionstore.Store
	deps     Deps
	root     int64
	client   *loopClient
	sessions *loopSessions
	notified []string
}

type loopClient struct {
	llm.Client
	t         *testing.T
	responses []string
	desks     []string
	calls     int
	calledAt  []time.Time
	closes    int
	err       error
}

type loopSessions struct {
	board []Card
	moves []string
	err   error
}

func TestAssistantLoopTurns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		responses []string
		reason    string
		says      []string
	}{
		{"happy", "hello", []string{sayAnswer("hello")}, "assistant_turn", []string{"hello"}},
		{"empty speech", "hello", []string{sayAnswer("")}, "assistant_turn", nil},
		{"blank speech", "hello", []string{sayAnswer(" \n\t")}, "assistant_turn", nil},
		{"reask", "hello", []string{"bad", "bad", sayAnswer("fixed")}, "assistant_turn", []string{"fixed"}},
		{"failed", "hello", []string{"bad", "bad", "bad"}, "assistant_turn_failed", []string{"⚠️ turn failed:"}},
		{"command", "/stop", nil, "assistant_ignored_command", nil},
		{"unknown session", "hello", []string{
			actionAnswer(`{"send":{"session":999,"text":"go"}}`), sayAnswer("corrected"),
		}, "assistant_turn", []string{"corrected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLoopFixture(t, tc.responses...)
			f.enqueue(tc.input)
			require.NoError(t, Run(t.Context(), f.deps, f.root))
			var state, reason string
			require.NoError(
				t,
				f.db.QueryRowContext(t.Context(), `SELECT state, resolution_reason FROM session_inbox`).
					Scan(&state, &reason),
			)
			assert.Equal(t, "handled", state)
			assert.Equal(t, tc.reason, reason)
			require.Len(t, f.notified, len(tc.says))

			for i, say := range tc.says {
				assert.Contains(t, f.notified[i], say)
			}

			assert.Equal(t, len(tc.responses), f.client.calls)
			assert.Equal(t, f.client.calls, f.client.closes)
			record, err := f.deps.Store.GetSession(t.Context(), f.root)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
			messages, err := f.store.LoadActiveMessages(t.Context(), f.root)
			require.NoError(t, err)
			assert.Empty(t, messages)
		})
	}
}

func TestAssistantLoopReadAndSlots(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, `{"say":"checking","focus":"saved focus","stickers":{"wait":"jobs"},"diary":{"key":"fact"},
		"do":[{"read":{"session":244,"lines":3}},{"read":{"session":244,"lines":1}}]}`, sayAnswer("read result"))
	f.enqueue("read it")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.Len(t, f.client.desks, 2)
	assert.Contains(t, f.client.desks[1], "saved focus")
	assert.Contains(t, f.client.desks[1], "key: fact")
	assert.Contains(t, f.client.desks[1], "- wait: jobs")
	assert.Contains(t, f.client.desks[1], "tail 3")
	assert.Contains(t, f.client.desks[1], "tail 1")
	assert.Equal(t, []string{"checking", "read result"}, f.notified)
}

func TestAssistantLoopRetainsFullTextOutsideDesk(t *testing.T) {
	t.Parallel()
	user, say := strings.Repeat("я", eventTextLimit+1000), strings.Repeat("ж", 8000)
	f := newLoopFixture(t, sayAnswer(say), sayAnswer("done"))
	f.enqueue(user)
	f.enqueue("next turn")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.Len(t, f.client.desks, 2)
	for _, desk := range f.client.desks {
		assert.NotContains(t, desk, user)
		assert.NotContains(t, desk, say)
		assert.Contains(t, desk, "[…]")
	}

	assert.Equal(t, []string{say, "done"}, f.notified)
	lines, err := f.deps.Memory.VisibleTape(t.Context(), f.root, 40, 0)
	require.NoError(t, err)
	require.Len(t, lines, 4)
	assert.Equal(t, "user: «"+user+"»", lines[0].Text)
	assert.Equal(t, "say: «"+say+"»", lines[1].Text)
}

func TestAssistantLoopActionsAndFailure(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, actionAnswer(`{"open":{"project":"devbox","text":"new"}},
		{"send":{"session":244,"text":"go"}},{"stop":{"session":244}}`))
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"open devbox new", "send 244 go", "stop 244"}, f.sessions.moves)
	lines, err := f.deps.Memory.VisibleTape(t.Context(), f.root, 40, 0)
	require.NoError(t, err)
	require.Len(t, lines, 4)
	assert.Equal(t, `→ devbox (245): "new"`, lines[3].Text)

	f.client.responses = append(
		f.client.responses,
		actionAnswer(`{"send":{"session":244,"text":"go"}}`),
		sayAnswer("could not send"),
	)
	f.sessions.err = errors.New("stopping")
	f.enqueue("try again")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	var kind string
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT json_extract(attributes, '$.assistant_event')
		FROM session_inbox WHERE source = 'agent'`).Scan(&kind))
	assert.Equal(t, "action_failed", kind)
	assert.Contains(t, f.client.desks[2], "stopping")
	assert.Equal(t, []string{"could not send"}, f.notified)
}

func TestAssistantLoopBindsStickerToOpenedSession(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		`{"say":null,"do":[{"open":{"project":"devbox","text":"new"}}],
			"stickers":{"wait.devbox":{"text":"tell when done","on":"devbox"}}}`,
		`{"say":"ok","stickers":{"wait.devbox":"tell when done, briefly"}}`,
	)
	f.enqueue("start")
	f.enqueue("next")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
	require.NoError(t, err)
	assert.Equal(t, []Sticker{{Key: "wait.devbox", Text: "tell when done, briefly", On: "245"}}, desk.Stickers)
	assert.Contains(t, f.client.desks[1], "- wait.devbox @245: tell when done")
}

func TestAssistantLoopCommitRollback(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, sayAnswer("lost"))
	f.enqueue("hello")
	f.deps.SayTx = func(context.Context, *sql.Tx, string) error { return errors.New("outbox unavailable") }
	require.ErrorContains(t, Run(t.Context(), f.deps, f.root), "outbox unavailable")
	inputs, err := f.deps.Store.ListPending(t.Context(), f.root)
	require.NoError(t, err)
	assert.Len(t, inputs, 1)
	assert.Empty(t, f.notified)
	var turns int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM assistant_turns`).Scan(&turns))
	assert.Zero(t, turns)
	desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
	require.NoError(t, err)
	assert.True(t, desk.LastTurnAt.IsZero())
}

func TestAssistantLoopForgetKeepsTurnUntilItEnds(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		`{"say":null,"do":[{"read":{"session":244,"lines":1}}],"forget":[[1,999]]}`,
		`{"say":"done","forget":[[1,999]]}`,
		sayAnswer("next"),
	)
	f.enqueue("check it")
	f.enqueue("again")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.Len(t, f.client.desks, 3)
	assert.Contains(t, f.client.desks[1], "user: «check it»", "the turn's question survives a forget while it reads")
	assert.NotContains(t, f.client.desks[2], "read devbox (244)", "the spent turn is hidden once it ends")
	assert.Contains(t, f.client.desks[2], "user: «check it»", "the dialog tail stays pinned")
	assert.Contains(t, f.client.desks[2], "say: «done»", "a forget never hides lines the model has not seen")
	assert.NotContains(t, strings.Join(f.client.desks, ""), "! error")
}

func TestAssistantLoopReadLimitAndFIFO(t *testing.T) {
	t.Parallel()
	read := actionAnswer(`{"read":{"session":244,"lines":1}}`)
	f := newLoopFixture(t, read, read, read, sayAnswer("first answer"), sayAnswer("second input"))
	f.enqueue("first")
	f.enqueue("second")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, 5, f.client.calls)
	assert.Contains(t, f.client.desks[3], "! error: no steps left in this turn: answer now, without read")
	assert.Contains(t, f.client.desks[4], "## event (line")
	assert.Contains(t, f.client.desks[4], "user: «second»\n\nAnswer")
	assert.Equal(t, []string{"first answer", "second input"}, f.notified)
}

func newLoopFixture(t *testing.T, responses ...string) *loopFixture {
	t.Helper()
	db, sessions, memory, root := assistantFixture(t)
	_, err := db.ExecContext(t.Context(), `CREATE TABLE test_replies (id INTEGER PRIMARY KEY, text TEXT NOT NULL)`)
	require.NoError(t, err)
	f := &loopFixture{
		t: t, db: db, store: sessions, root: root, client: &loopClient{t: t, responses: responses},
		sessions: &loopSessions{board: []Card{{ID: 244, Project: "devbox", Status: "done"}}},
	}
	f.deps = Deps{
		Store: sessions, Memory: memory, Sessions: f.sessions, Now: func() time.Time { return time.Now().UTC() },
		NewClient: func(model string) (llm.Client, error) {
			assert.Equal(t, "model", model)

			return f.client, nil
		},
		SayTx: func(ctx context.Context, tx *sql.Tx, text string) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO test_replies (text) VALUES (?)`, text)

			return err
		},
		Notify: func() {
			var text string
			require.NoError(
				t,
				db.QueryRowContext(t.Context(), `SELECT text FROM test_replies ORDER BY id DESC LIMIT 1`).Scan(&text),
			)
			f.notified = append(f.notified, text)
		},
		Enqueue: func(ctx context.Context, kind string, id int64, text string) error {
			_, err := sessions.Enqueue(ctx, sessionstore.Input{
				SessionID: root, Source: sessionstore.InputSourceAgent,
				Content: text, Attributes: map[string]any{"assistant_event": kind, "session_id": id},
			})

			return err
		},
	}

	return f
}

func (f *loopFixture) enqueue(text string) {
	f.t.Helper()
	_, err := f.store.Enqueue(
		f.t.Context(),
		sessionstore.Input{SessionID: f.root, Source: sessionstore.InputSourceUser, Content: text},
	)
	require.NoError(f.t, err)
}

func (c *loopClient) Chat(
	_ context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.calls++
	c.calledAt = append(c.calledAt, time.Now())
	assert.Equal(c.t, registry.AssistantPrompt, system)
	require.Len(c.t, messages, 1)
	assert.Equal(c.t, "user", messages[0].Role)
	assert.Empty(c.t, tools)
	c.desks = append(c.desks, messages[0].Content)
	if c.err != nil {
		return nil, c.err
	}

	require.LessOrEqual(c.t, c.calls, len(c.responses))

	return &llmwire.Response{Text: c.responses[c.calls-1]}, nil
}

func (c *loopClient) Close() error { c.closes++; return nil }

func (s *loopSessions) Board(context.Context, time.Time) ([]Card, error) { return s.board, nil }
func (s *loopSessions) Read(_ context.Context, _ int64, lines int) (string, error) {
	return fmt.Sprintf("tail %d", lines), nil
}

func (s *loopSessions) Open(_ context.Context, project, text string) (Card, error) {
	s.moves = append(s.moves, "open "+project+" "+text)

	return Card{ID: 245, Project: project}, s.err
}

func (s *loopSessions) Send(_ context.Context, id int64, text string) error {
	s.moves = append(s.moves, fmt.Sprintf("send %d %s", id, text))

	return s.err
}

func (s *loopSessions) Stop(_ context.Context, id int64) error {
	s.moves = append(s.moves, fmt.Sprintf("stop %d", id))

	return s.err
}

func sayAnswer(say string) string {
	return fmt.Sprintf(`{"say":%q}`, say)
}

func actionAnswer(actions string) string {
	return `{"say":null,"do":[` + actions + `]}`
}

func TestAssistantLoopPinsDialogTail(t *testing.T) {
	t.Parallel()
	forgetAll := `{"say":"ok","forget":[[1,999]]}`
	f := newLoopFixture(t, forgetAll, forgetAll, forgetAll, forgetAll, forgetAll)
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		f.enqueue(text)
	}

	require.NoError(t, Run(t.Context(), f.deps, f.root))
	last := f.client.desks[4]
	assert.NotContains(t, last, "user: «one»", "older dialog lines can be forgotten")
	for _, text := range []string{"user: «two»", "user: «three»", "user: «four»", "user: «five»"} {
		assert.Contains(t, last, text, "the last six dialog lines stay visible")
	}
}

func TestAssistantLoopNotesWhatWasAlreadyTold(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		actionAnswer(`{"read":{"session":244,"lines":5}}`),
		sayAnswer("CI is green"),
		`{"say":null}`,
	)
	f.enqueue("status?")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.NoError(t, f.deps.Enqueue(t.Context(), "answered", 244, `← devbox (244) answered: "CI is green"`))
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.Len(t, f.client.desks, 3)
	assert.Contains(t, f.client.desks[2], "Host note: after reading this session at")
	assert.Contains(t, f.client.desks[2], "say: «CI is green»")
	assert.Equal(t, []string{"CI is green"}, f.notified, "the second answer stays silent")
}

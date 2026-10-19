package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	responses []*llmwire.Response
	requests  [][]llmwire.Message
	calls     int
	calledAt  []time.Time
	closes    int
	err       error
	failFrom  int
}

type loopSessions struct {
	cards   []Card
	hits    []Hit
	moves   []string
	err     error
	openErr error
}

func TestAssistantLoopTurns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		responses []*llmwire.Response
		reason    string
		says      []string
	}{
		{"happy", "hello", []*llmwire.Response{say("hello")}, "assistant_turn", []string{"hello"}},
		{"silence", "hello", []*llmwire.Response{say("")}, "assistant_turn", nil},
		{"blank speech", "hello", []*llmwire.Response{say(" \n\t")}, "assistant_turn", nil},
		{"command", "/stop", nil, "assistant_ignored_command", nil},
		{"invalid target corrected", "hello", []*llmwire.Response{
			act(use(toolSend, `{"session":"999","text":"go"}`)), say("corrected"),
		}, "assistant_turn", []string{"corrected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLoopFixture(t, tc.responses...)
			f.enqueue(tc.input)
			require.NoError(t, Run(t.Context(), f.deps, f.root))
			var state, reason string
			require.NoError(t, f.db.QueryRowContext(t.Context(),
				`SELECT state, resolution_reason FROM session_inbox`).Scan(&state, &reason))
			assert.Equal(t, "handled", state)
			assert.Equal(t, tc.reason, reason)
			assert.Equal(t, tc.says, f.notified)
			assert.Equal(t, len(tc.responses), f.client.calls)
			assert.Equal(t, f.client.calls, f.client.closes)
			assert.Empty(t, f.sessions.moves)
			record, err := f.deps.Store.GetSession(t.Context(), f.root)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
			messages, err := f.store.LoadActiveMessages(t.Context(), f.root)
			require.NoError(t, err)
			assert.Empty(t, messages)
		})
	}
}

func TestAssistantLoopLookThenReplyKeepsTapeOrder(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolRead, `{"session":244,"count":3}`), use(toolStop, `{"session":244}`)),
		say("всё прочитал"),
	)
	f.enqueue("что там?")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"всё прочитал"}, f.notified)
	assert.Equal(t, []string{"stop 244"}, f.sessions.moves)
	second := f.client.requests[1]
	assert.Equal(t, "read devbox (244):\ntail 3", second[2].Content)
	assert.Equal(t, recordedResult, second[3].Content)
	assert.Equal(t, []string{
		"user: «что там?»", "read devbox (244)", "say: «всё прочитал»", "⏹ stopped devbox (244)",
	}, f.tapeTexts())
	assert.True(t, strings.HasPrefix(f.client.requests[0][0].Content, "now: "))
	assert.Contains(t, f.client.requests[0][0].Content, "244 devbox · done")
}

func TestAssistantLoopConsolidatesSpeech(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		withText("Смотрю.", use(toolRead, `{"session":244,"count":1}`)),
		say("Готово: всё зелёное."),
		withText("Всё зелёное.", use(toolRead, `{"session":244,"count":1}`)),
	)
	f.enqueue("проверь")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"Всё зелёное."}, f.notified, "a consolidation's tool calls are ignored")
	consolidation := f.client.requests[2]
	require.Len(t, consolidation, 5, "a branch of the main conversation plus one request")
	assert.Contains(t, consolidation[4].Content, "Смотрю.")
	assert.Contains(t, consolidation[4].Content, "Готово: всё зелёное.")
	assert.Equal(t, 1, strings.Count(strings.Join(f.tapeTexts(), "\n"), "say: "))
	assert.Contains(t, f.tapeTexts(), "say: «Всё зелёное.»")
}

func TestAssistantLoopConsolidationFallsBackToJoin(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, withText("Раз.", use(toolFocusList, `{}`)), say("Два."), say(" "))
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"Раз.\n\nДва."}, f.notified)
}

func TestAssistantLoopGuardAsksForFocus(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolSend, `{"session":244,"text":"почини redis"}`)),
		say("Проверю redis."),
		withText("guard chatter", use(toolFocus, `{"text":"Redis diagnosis","sessions":["244"]}`)),
		say("still chatter"),
	)
	f.enqueue("redis лежит")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"Проверю redis."}, f.notified, "guard text never reaches the user")
	assert.Equal(t, []string{"send 244 почини redis"}, f.sessions.moves)
	guardRequest := f.client.requests[2]
	assert.Contains(t, guardRequest[len(guardRequest)-1].Content, "send to 244 devbox has no focus item")
	focus, err := f.deps.Memory.Focus(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, []string{"244"}, focus[0].Sessions)
	assert.Equal(t, []string{"main", "main", "guard", "guard"}, f.callPhases())
	assert.Equal(t, []string{"ok"}, f.callOutcomes())
	assert.NotContains(t, strings.Join(f.tapeTexts(), "\n"), "chatter")
}

func TestAssistantLoopGuardGivesUp(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, act(use(toolSend, `{"session":244,"text":"go"}`)), say("ok"), say(""), say(""))
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"ok"}, f.notified)
	assert.Equal(t, []string{"send 244 go"}, f.sessions.moves)
	assert.Equal(t, []string{"main", "main", "guard", "guard"}, f.callPhases())
	assert.Equal(t, []string{"guard_unresolved"}, f.callOutcomes())
}

func TestAssistantLoopGuardRefusesNewWork(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolSearch, `{"query":"redis"}`)),
		say("Не нашёл."),
		act(use(toolOpen, `{"project":"devbox","about":"why","text":"go"}`),
			use(toolRemember, `{"text":"Искал redis: ничего","until":"2999-01-01"}`)),
		say(""),
	)
	f.enqueue("помнишь redis?")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	results := f.client.requests[3]
	assert.Equal(t, "error: "+errFinalReply.Error(), results[len(results)-2].Content)
	assert.True(t, results[len(results)-2].ToolError)
	assert.Empty(t, f.sessions.moves)
	memory, err := f.deps.Memory.Memory(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, memory, 1)
	assert.Equal(t, "2999-01-01", memory[0].Until)
}

func TestAssistantLoopOpenHandleResolves(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, act(
		use(toolOpen, `{"project":"devbox","about":"MinIO for local S3","text":"добавь minio"}`),
		use(toolSend, `{"session":"new-1-1","text":"и проверь"}`),
		use(toolFocus, `{"text":"MinIO setup","sessions":["new-1-1"]}`),
	), say("Добавлю MinIO."))
	f.enqueue("добавь minio")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"open devbox добавь minio MinIO for local S3", "send 245 и проверь"}, f.sessions.moves)
	focus, err := f.deps.Memory.Focus(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, []string{"245"}, focus[0].Sessions)
	assert.Equal(t, []string{
		"user: «добавь minio»",
		"say: «Добавлю MinIO.»",
		`→ devbox (245): "добавь minio"`,
		`→ devbox (245): "и проверь"`,
	}, f.tapeTexts())
	assert.Contains(t, f.client.requests[1][2].Content, "handle new-1-1")
}

func TestAssistantLoopFailedOpenSkipsItsSends(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(
			use(toolOpen, `{"project":"devbox","about":"why","text":"start"}`),
			use(toolSend, `{"session":"new-1-1","text":"then this"}`),
			use(toolFocus, `{"text":"Both","sessions":["new-1-1","244"]}`),
			use(toolSend, `{"session":244,"text":"other"}`),
		),
		say("Запускаю."),
		say("Не вышло открыть."),
	)
	f.sessions.openErr = errors.New("disk full")
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"open devbox start why", "send 244 other"}, f.sessions.moves)
	focus, err := f.deps.Memory.Focus(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, []string{"244"}, focus[0].Sessions)
	var events []string
	rows, err := f.db.QueryContext(t.Context(), `SELECT raw_content FROM session_inbox WHERE source = 'agent'`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var text string
		require.NoError(t, rows.Scan(&text))
		events = append(events, text)
	}
	require.NoError(t, rows.Err())
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "open devbox failed: disk full")
	assert.Contains(t, events[0], `skipped send to it: "then this"`)
	assert.Equal(t, []string{"Запускаю.", "Не вышло открыть."}, f.notified)
}

func TestAssistantLoopNeverRebindsAnEarlierHandle(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolOpen, `{"project":"devbox","about":"why","text":"start"}`),
			use(toolFocus, `{"text":"Start","sessions":["new-1-1"]}`)),
		say("ok"),
		act(use(toolSend, `{"session":"new-1-1","text":"again"}`),
			use(toolFocus, `{"text":"Again","sessions":["new-1-1"]}`)),
		say("ok"),
	)
	f.enqueue("start")
	f.enqueue("again")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	results := f.client.requests[3]
	assert.Contains(t, results[len(results)-2].Content, "neither a session id nor a handle opened in this turn")
	assert.Contains(t, results[len(results)-1].Content, "neither a session id nor a handle opened in this turn")
	assert.Equal(t, []string{"open devbox start why"}, f.sessions.moves)
}

func TestAssistantLoopEditsNotesByTemporaryID(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolRemember, `{"text":"first draft"}`), use(toolFocus, `{"text":"draft","sessions":[]}`)),
		act(use(toolRewriteMemory, `{"id":"m-new-1","text":"Пишет по-русски"}`),
			use(toolUpdateFocus, `{"id":"f-new-1","text":"Final","sessions":["244"]}`),
			use(toolRemember, `{"text":"to drop"}`), use(toolForgetMemory, `{"id":"m-new-2"}`)),
		say("ok"),
		act(use(toolDoneFocus, `{"id":"f1"}`), use(toolRewriteMemory, `{"id":"1","text":"Пишет кратко"}`)),
		say(""),
	)
	f.enqueue("запомни")
	f.enqueue("закрой")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Contains(t, f.client.requests[3][0].Content, "[m1] (permanent) Пишет по-русски")
	assert.Contains(t, f.client.requests[3][0].Content, "[f1] now · Final → 244")
	memory, err := f.deps.Memory.Memory(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, memory, 1)
	assert.Equal(t, "Пишет кратко", memory[0].Text)
	focus, err := f.deps.Memory.Focus(t.Context(), f.root)
	require.NoError(t, err)
	assert.Empty(t, focus)
}

func TestAssistantLoopRoundCapIgnoresStrayCalls(t *testing.T) {
	t.Parallel()
	responses := []*llmwire.Response{act(use(toolSend, `{"session":244,"text":"go"}`))}
	for range mainCalls - 1 {
		responses = append(responses, act(use(toolFocusList, `{}`)))
	}
	responses = append(responses,
		withText("Хватит.", use(toolRead, `{"session":244,"count":1}`)),
		act(use(toolFocus, `{"text":"Wait","sessions":["244"]}`)),
		say(""),
	)
	f := newLoopFixture(t, responses...)
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"Хватит."}, f.notified)
	final := f.client.requests[mainCalls]
	assert.Equal(t, roundCapNote, final[len(final)-1].Content)
	guard := f.client.requests[mainCalls+1]
	ignored := guard[len(guard)-2]
	assert.Equal(t, llmwire.RoleTool, ignored.Role)
	assert.Equal(t, ignoredToolCall, ignored.Content)
	assert.Contains(t, guard[len(guard)-1].Content, "has no focus item")
	assert.NotContains(t, f.tapeTexts(), "read devbox (244)", "the stray read never ran")
	assert.Equal(t, []string{"ok"}, f.callOutcomes())
}

func TestAssistantLoopNotesWhatWasAlreadyTold(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolRead, `{"session":244,"count":5}`)),
		say("CI is green"),
		say(""),
	)
	f.enqueue("status?")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.NoError(t, f.deps.Enqueue(t.Context(), "answered", 244, `← devbox (244) answered: "CI is green"`))
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	desk := f.client.requests[2][0].Content
	assert.Contains(t, desk, "Host note: after reading this session at")
	assert.Contains(t, desk, "say: «CI is green»")
	assert.Contains(t, desk, "## conversation (last 2)")
	assert.Equal(t, []string{"CI is green"}, f.notified, "the second answer stays silent")
}

func TestAssistantLoopAnsweredSessionNeedsFocusUpdate(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t,
		act(use(toolSend, `{"session":244,"text":"go"}`), use(toolFocus, `{"text":"Wait CI","sessions":["244"]}`)),
		say(""),
		say("CI зелёный."),
		act(use(toolDoneFocus, `{"id":"f1"}`)),
		say(""),
	)
	f.enqueue("go")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	require.NoError(t, f.deps.Enqueue(t.Context(), "answered", 244, `← devbox (244) answered: "green"`))
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	guard := f.client.requests[3]
	assert.Contains(t, guard[len(guard)-1].Content, "session 244 answered, but focus item f1")
	assert.Equal(t, []string{"CI зелёный."}, f.notified)
}

func TestAssistantLoopKeepsInputOrderAndFullText(t *testing.T) {
	t.Parallel()
	user, reply := strings.Repeat("я", eventTextLimit+1000), strings.Repeat("ж", 3000)
	f := newLoopFixture(t, say(reply), say("второй"))
	f.enqueue(user)
	f.enqueue("второй вопрос")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{reply, "второй"}, f.notified)
	assert.NotContains(t, f.client.requests[0][0].Content, user, "the event keeps its head and tail")
	second := f.client.requests[1][0].Content
	assert.Contains(t, second, "user: «второй вопрос»")
	assert.NotContains(t, second, reply, "a conversation line is clipped on the desk")
	assert.Equal(t, []string{"user: «" + user + "»", "say: «" + reply + "»", "user: «второй вопрос»", "say: «второй»"},
		f.tapeTexts(), "the tape keeps full text")
}

func TestAssistantLoopCommitRollback(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, act(use(toolRemember, `{"text":"lost"}`)), say("lost"))
	f.enqueue("hello")
	f.deps.SayTx = func(context.Context, *sql.Tx, string) error { return errors.New("outbox unavailable") }
	require.ErrorContains(t, Run(t.Context(), f.deps, f.root), "outbox unavailable")
	inputs, err := f.deps.Store.ListPending(t.Context(), f.root)
	require.NoError(t, err)
	assert.Len(t, inputs, 1)
	assert.Empty(t, f.notified)
	assert.Empty(t, f.callPhases())
	memory, err := f.deps.Memory.Memory(t.Context(), f.root)
	require.NoError(t, err)
	assert.Empty(t, memory)
}

func TestAssistantToolsMatchPrompt(t *testing.T) {
	t.Parallel()
	var names []string
	for _, tool := range Tools() {
		names = append(names, tool.Name)
		assert.True(t, json.Valid(tool.Parameters), tool.Name)
		assert.Contains(t, registry.AssistantPrompt, "`"+tool.Name+"(", tool.Name)
	}

	var mentioned []string
	for _, match := range regexp.MustCompile("`([a-z_]+)\\(").FindAllStringSubmatch(registry.AssistantPrompt, -1) {
		if !slices.Contains(mentioned, match[1]) {
			mentioned = append(mentioned, match[1])
		}
	}

	assert.ElementsMatch(t, names, mentioned, "every call-shaped mention in the prompt is a registered tool")

	for _, banned := range []string{`"say"`, `"do"`, "`forget`", "JSON object", "forget\":"} {
		assert.NotContains(t, registry.AssistantPrompt, banned)
	}
}

func newLoopFixture(t *testing.T, responses ...*llmwire.Response) *loopFixture {
	t.Helper()
	db, sessions, memory, root := assistantFixture(t)
	_, err := db.ExecContext(t.Context(), `CREATE TABLE test_replies (id INTEGER PRIMARY KEY, text TEXT NOT NULL)`)
	require.NoError(t, err)
	f := &loopFixture{
		t: t, db: db, store: sessions, root: root, client: &loopClient{t: t, responses: responses},
		sessions: &loopSessions{cards: []Card{{ID: 244, Project: "devbox", Status: "done"}}},
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
			require.NoError(t, db.QueryRowContext(t.Context(),
				`SELECT text FROM test_replies ORDER BY id DESC LIMIT 1`).Scan(&text))
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
	_, err := f.store.Enqueue(f.t.Context(),
		sessionstore.Input{SessionID: f.root, Source: sessionstore.InputSourceUser, Content: text})
	require.NoError(f.t, err)
}

func (f *loopFixture) tapeTexts() []string {
	f.t.Helper()
	lines, err := f.deps.Memory.RecentTape(f.t.Context(), f.root, 100)
	require.NoError(f.t, err)
	texts := make([]string, 0, len(lines))
	for _, line := range lines {
		texts = append(texts, line.Text)
	}

	return texts
}

func (f *loopFixture) callPhases() []string {
	return f.column(`SELECT phase FROM assistant_turns ORDER BY id`)
}

func (f *loopFixture) callOutcomes() []string {
	return f.column(`SELECT DISTINCT outcome FROM assistant_turns ORDER BY id`)
}

func (f *loopFixture) column(query string) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(f.t.Context(), query)
	require.NoError(f.t, err)
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(f.t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(f.t, rows.Err())

	return values
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
	assert.Equal(c.t, Tools(), tools, "the tool list never changes within a turn")
	assert.Equal(c.t, llmwire.RoleUser, messages[0].Role)
	c.requests = append(c.requests, slices.Clone(messages))

	if c.err != nil && c.calls >= c.failFrom {
		return nil, c.err
	}

	require.LessOrEqual(c.t, c.calls, len(c.responses), "unexpected model call %d", c.calls)

	return c.responses[c.calls-1], nil
}

func (c *loopClient) Close() error { c.closes++; return nil }

func (s *loopSessions) Recent(context.Context, int) ([]Card, int, error) {
	return s.cards, len(s.cards), nil
}

func (s *loopSessions) List(_ context.Context, offset, limit int, _ string) ([]Card, int, error) {
	return s.cards[min(offset, len(s.cards)):min(offset+limit, len(s.cards))], len(s.cards), nil
}

func (s *loopSessions) Projects(context.Context) ([]Project, error) {
	return []Project{{Name: "devbox", Key: "devbox", Place: "repo ~/devbox"}}, nil
}

func (s *loopSessions) Search(context.Context, string) ([]Hit, error) { return s.hits, nil }

func (s *loopSessions) Around(_ context.Context, ref string, count int) (string, error) {
	return fmt.Sprintf("around %s %d", ref, count), nil
}

func (s *loopSessions) Read(_ context.Context, _ int64, count int) (string, error) {
	return fmt.Sprintf("tail %d", count), nil
}

func (s *loopSessions) Target(_ context.Context, id int64) (Card, error) {
	for _, card := range s.cards {
		if card.ID == id {
			return card, nil
		}
	}

	return Card{}, fmt.Errorf("session %d is not a work session you can address", id)
}

func (s *loopSessions) CheckProject(_ context.Context, project string) error {
	if project == "bad" {
		return errors.New("bad project")
	}

	return nil
}

func (s *loopSessions) Open(_ context.Context, project, text, about string) (Card, error) {
	s.moves = append(s.moves, strings.Join([]string{"open", project, text, about}, " "))
	if s.openErr != nil {
		return Card{}, s.openErr
	}

	card := Card{ID: 245, Project: project, Status: "working"}
	s.cards = append(s.cards, card)

	return card, nil
}

func (s *loopSessions) Send(_ context.Context, id int64, text string) error {
	s.moves = append(s.moves, fmt.Sprintf("send %d %s", id, text))

	return s.err
}

func (s *loopSessions) Stop(_ context.Context, id int64) error {
	s.moves = append(s.moves, fmt.Sprintf("stop %d", id))

	return s.err
}

func say(text string) *llmwire.Response {
	return &llmwire.Response{Text: text, FinishType: llmwire.FinishStop}
}

func act(calls ...llmwire.ToolCall) *llmwire.Response {
	return withText("", calls...)
}

func withText(text string, calls ...llmwire.ToolCall) *llmwire.Response {
	for i := range calls {
		calls[i].ID = fmt.Sprintf("call-%s-%d", calls[i].Name, i)
	}

	return &llmwire.Response{Text: text, ToolCalls: calls, FinishType: llmwire.FinishToolCalls}
}

func use(name, args string) llmwire.ToolCall {
	return llmwire.ToolCall{Name: name, Arguments: []byte(args)}
}

func TestAssistantLoopKeepsApplyingAfterABookkeepingError(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(t, act(
		use(toolOpen, `{"project":"devbox","about":"why","text":"start"}`),
		use(toolSend, `{"session":244,"text":"other"}`),
		use(toolFocus, `{"text":"Both","sessions":["new-1-1","244"]}`),
	), say("ok"))
	f.sessions.openErr = errors.New("disk full")
	f.deps.Enqueue = func(context.Context, string, int64, string) error { return errors.New("inbox unavailable") }
	f.enqueue("go")
	require.ErrorContains(t, Run(t.Context(), f.deps, f.root), "inbox unavailable")
	assert.Equal(t, []string{"open devbox start why", "send 244 other"}, f.sessions.moves,
		"a failed report never cancels the effects after it")
}

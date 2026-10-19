package assistant

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

const testOwner = "manager-a"

func TestAssistantStoreSendsAndCalls(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	input, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "hello"},
	)
	require.NoError(t, err)
	at := time.Now().UTC()
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, RecordSendTx(ctx, tx,
			Send{InputID: input.Input.ID, AssistantSessionID: root, SessionID: root, CreatedAt: at}))

		return RecordCallTx(ctx, tx, Call{
			SessionID: root, InputID: input.Input.ID, Number: 1, Phase: phaseMain,
			Request: "desk", Response: "raw", Outcome: "ok", CreatedAt: at,
		})
	}))
	sends, err := memory.UnobservedSends(ctx, root)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	assert.Equal(t, at, sends[0].CreatedAt)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		won, err := MarkObservedTx(ctx, tx, input.Input.ID, at)
		require.NoError(t, err)
		assert.True(t, won)
		won, err = MarkObservedTx(ctx, tx, input.Input.ID, at.Add(time.Second))
		require.NoError(t, err)
		assert.False(t, won)

		return nil
	}))
	sends, err = memory.UnobservedSends(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, sends)
	var phase, request, outcome string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT phase, request, outcome FROM assistant_turns`).
		Scan(&phase, &request, &outcome))
	assert.Equal(t, []string{phaseMain, "desk", "ok"}, []string{phase, request, outcome})

	for _, query := range []string{
		`INSERT INTO assistant_tape (session_id, at, kind, text) VALUES (1, CURRENT_TIMESTAMP, 'invalid', '')`,
		`INSERT INTO assistant_turns (session_id, input_id, call, phase, request, response, outcome, created_at)
			VALUES (1, 1, 1, 'main', '', '', 'invalid', CURRENT_TIMESTAMP)`,
		`INSERT INTO assistant_memory (session_id, text, until, created_at, updated_at)
			VALUES (1, 'x', 'tomorrow', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO assistant_focus (session_id, text, sessions, created_at, updated_at)
			VALUES (1, 'x', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		_, err := db.ExecContext(ctx, query)
		require.ErrorContains(t, err, "CHECK", query)
	}
}

func TestAssistantTapeRollsBackWithItsIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, memory, root := assistantFixture(t)
	abort := errors.New("abort turn")
	err := sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := AppendTapeTx(ctx, tx, root, Line{At: time.Now().UTC(), Kind: sayKind, Text: "say: «потеряно»"})
		require.NoError(t, err)

		return abort
	})
	require.ErrorIs(t, err, abort)
	hits, err := memory.Search(ctx, Scope{Root: root, Owner: testOwner}, "потеряно")
	require.NoError(t, err)
	assert.Empty(t, hits)
	lines, err := memory.RecentTape(ctx, root, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)
}

func TestAssistantSearchFindsInflectedWordsAndCompactedFinals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	_, final := answered(t, sessions, work, "Почини права", "Причина: проблема кеширования прав на volume")
	_, err := db.ExecContext(ctx, `UPDATE messages SET compacted_at = CURRENT_TIMESTAMP WHERE id = ?`, final)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := AppendTapeTx(ctx, tx, root, Line{At: now, Kind: sayKind, Text: "say: «включу кеширование»"})
		require.NoError(t, err)
		_, err = AppendTapeTx(ctx, tx, root, Line{At: now, Kind: moveKind, Text: "→ кешированию не место"})
		require.NoError(t, err)
		_, err = insertMemoryTx(ctx, tx, root, Memory{Text: "Кешированный ответ хранится сутки"}, now)

		return err
	}))
	scope := Scope{Root: root, Owner: testOwner}

	hits, err := memory.Search(ctx, scope, "кеширован*")
	require.NoError(t, err)
	refs := hitRefs(hits)
	assert.Contains(t, refs, "msg "+itoa(final), "a compacted final is still found")
	assert.Len(t, refs, 3, "the final, the say line and the memory note: %v", refs)
	hits, err = memory.Search(ctx, scope, "кешированию")
	require.NoError(t, err)
	assert.Empty(t, hits, "a move line is never indexed")
	hits, err = memory.Search(ctx, scope, "кеширование")
	require.NoError(t, err)
	require.Len(t, hits, 1, "no stemming: only the exact form matches")
	assert.Equal(t, "say", hits[0].Kind)
	assert.Contains(t, hits[0].Snippet, "[кеширование]")
	_, err = memory.Search(ctx, scope, `" * "`)
	require.ErrorIs(t, err, ErrEmptyQuery)
	hits, err = memory.Search(ctx, scope, `NEAR(кеш*) OR "прав`)
	require.NoError(t, err, "model text is never FTS syntax")
	assert.NotEmpty(t, hits)
}

func TestAssistantSearchKeepsToAuthorizedRoots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	own := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	userMade := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	foreign := workSession(t, sessions, map[string]any{"manager_id": "manager-b"})
	child := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	surface := workSession(t, sessions, map[string]any{"manager_id": testOwner, "management_surface": true})
	assistantRoot := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	killed := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	for _, id := range []int64{own, userMade, foreign, child, surface, assistantRoot, killed} {
		answered(t, sessions, id, "найди секрет", "секрет найден")
	}

	_, err := db.ExecContext(ctx, `UPDATE sessions SET parent_id = ?, root_id = ? WHERE id = ?`, own, own, child)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET agent_type = 'assistant' WHERE id = ?`, assistantRoot)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET killed_at = CURRENT_TIMESTAMP, status = 'killed' WHERE id = ?`,
		killed)
	require.NoError(t, err)

	hits, err := memory.Search(ctx, Scope{Root: root, Owner: testOwner}, "секрет")
	require.NoError(t, err)
	sessionsFound := map[int64]bool{}
	for _, hit := range hits {
		sessionsFound[hit.Session] = true
	}
	assert.Equal(t, map[int64]bool{own: true, userMade: true}, sessionsFound)
	assert.Len(t, hits, 4, "the accepted request and the final of each root")

	for _, id := range []int64{foreign, child, surface, assistantRoot, killed} {
		ok, err := memory.Authorized(ctx, testOwner, id)
		require.NoError(t, err)
		assert.False(t, ok, id)
	}

	ok, err := memory.Authorized(ctx, testOwner, userMade)
	require.NoError(t, err)
	assert.True(t, ok, "a root the user created is addressable too")
	_, err = memory.Around(ctx, Scope{Root: root, Owner: testOwner}, "msg "+itoa(lastMessage(t, db, foreign)), 2)
	require.ErrorContains(t, err, "not a message of a session you can see")
}

func TestAssistantSearchSkipsNoiseAndRejectedRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	answered(t, sessions, work, "начало", "итог")
	_, err := sessions.Commit(ctx, sessionstore.Commit{SessionID: work, Messages: []*transcript.Message{
		{Role: "user", Content: "служебная заметка"},
		{Role: "assistant", Content: "служебный вызов", ToolCalls: []byte(`[{"ID":"1","Name":"bash"}]`)},
		{Role: "assistant", Content: "служебный обрыв", FinishType: "length", RejectedReason: "output_length"},
		{Role: "assistant", Content: "служебный ответ", FinishType: "stop"},
	}})
	require.NoError(t, err)
	scope := Scope{Root: root, Owner: testOwner}
	hits, err := memory.Search(ctx, scope, "служебн*")
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Contains(t, hits[0].Snippet, "ответ")
	var watermark, last int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT message_id FROM assistant_search_state`).Scan(&watermark))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages`).Scan(&last))
	assert.Equal(t, last, watermark)
}

func TestAssistantSearchCatchesUpInBatches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	messages := make([]*transcript.Message, 0, indexBatchSize+20)
	for range indexBatchSize + 20 {
		messages = append(messages, &transcript.Message{Role: "assistant", Content: "пакет", FinishType: "stop"})
	}
	_, err := sessions.Commit(ctx, sessionstore.Commit{SessionID: work, Messages: messages})
	require.NoError(t, err)
	hits, err := memory.Search(ctx, Scope{Root: root, Owner: testOwner}, "пакет")
	require.NoError(t, err)
	assert.Len(t, hits, searchHits)
	var indexed int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_search WHERE rowid % 4 = 0`).
		Scan(&indexed))
	assert.Equal(t, indexBatchSize+20, indexed)
}

func TestAssistantMemoryExpiryAndDeletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, memory, root := assistantFixture(t)
	now := time.Now().UTC()
	var expired, permanent int64
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		expired, err = insertMemoryTx(ctx, tx, root,
			Memory{Text: "искал старый отчёт", Until: now.AddDate(0, 0, -1).Format(time.DateOnly)}, now)
		require.NoError(t, err)
		permanent, err = insertMemoryTx(ctx, tx, root, Memory{Text: "отчёт по пятницам"}, now)

		return err
	}))
	desk, err := memory.LoadDesk(ctx, root, 0, now)
	require.NoError(t, err)
	require.Len(t, desk.Memory, 1)
	assert.Equal(t, permanent, desk.Memory[0].ID)
	all, err := memory.Memory(ctx, root)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	scope := Scope{Root: root, Owner: testOwner}
	hits, err := memory.Search(ctx, scope, "отчёт")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"m" + itoa(expired), "m" + itoa(permanent)}, hitRefs(hits))
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		return deleteMemoryTx(ctx, tx, root, permanent)
	}))
	hits, err = memory.Search(ctx, scope, "отчёт")
	require.NoError(t, err)
	assert.Equal(t, []string{"m" + itoa(expired)}, hitRefs(hits), "a deleted note is never found again")
}

func TestAssistantAroundShowsNeighbours(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, memory, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	answered(t, sessions, work, "первый вопрос", "первый ответ")
	_, middle := answered(t, sessions, work, "второй вопрос", "второй ответ")
	answered(t, sessions, work, "третий вопрос", "третий ответ")
	scope := Scope{Root: root, Owner: testOwner}
	text, err := memory.Around(ctx, scope, "msg "+itoa(middle), 1)
	require.NoError(t, err)
	assert.Contains(t, text, "user: второй вопрос")
	assert.Contains(t, text, "> msg "+itoa(middle))
	assert.Contains(t, text, "user: третий вопрос")
	assert.NotContains(t, text, "первый")
	var ids []int64
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		for _, text := range []string{"one", "two", "three"} {
			id, err := AppendTapeTx(ctx, tx, root, Line{At: time.Now().UTC(), Kind: observeKind, Text: text})
			require.NoError(t, err)
			ids = append(ids, id)
		}

		return nil
	}))
	text, err = memory.Around(ctx, scope, "line "+itoa(ids[1]), 1)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(text, "line "))
	_, err = memory.Around(ctx, scope, "line 999", 1)
	require.ErrorContains(t, err, "not on your tape")
	_, err = memory.Around(ctx, scope, "m12", 1)
	require.ErrorContains(t, err, "use \"msg <id>\"")
}

func TestAssistantWorkRoots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, memory, root := assistantFixture(t)
	touched := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	userMade := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	answered(t, sessions, userMade, strings.Repeat("длинный вопрос ", 20), "ответ")
	in, err := sessions.Enqueue(ctx, sessionstore.Input{
		SessionID: touched, Source: sessionstore.InputSourceUser,
		Content: "go",
	})
	require.NoError(t, err)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, RecordSendTx(ctx, tx,
			Send{InputID: in.Input.ID, AssistantSessionID: root, SessionID: touched, CreatedAt: time.Now().UTC()}))

		return SaveAboutTx(ctx, tx, touched, "why it exists", time.Now().UTC())
	}))
	scope := Scope{Root: root, Owner: testOwner}
	roots, err := memory.WorkRoots(ctx, scope, true)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, touched, roots[0].ID)
	assert.True(t, roots[0].Pending)
	assert.Equal(t, "why it exists", roots[0].About)
	roots, err = memory.WorkRoots(ctx, scope, false)
	require.NoError(t, err)
	require.Len(t, roots, 2)
	for _, work := range roots {
		if work.ID == userMade {
			assert.False(t, work.Touched)
			assert.Len(t, []rune(work.About), aboutFallbackChars)
			assert.True(t, strings.HasPrefix(work.About, "длинный вопрос длинный"))
		}
	}
}

func assistantFixture(t *testing.T) (*sql.DB, *sessionstore.Store, Store, int64) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "assistant.db")
	db, err := migrate.OpenDB(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, path))
	sessions := sessionstore.NewStore(db)
	project, err := sessions.GetOrCreateProject(ctx, "/project")
	require.NoError(t, err)
	root, err := sessions.CreateSession(ctx, project, "model", "", nil)
	require.NoError(t, err)

	return db, sessions, New(db), root.ID
}

func workSession(t *testing.T, sessions *sessionstore.Store, attrs map[string]any) int64 {
	t.Helper()
	record, err := sessions.CreateSession(t.Context(), 1, "model", "", attrs)
	require.NoError(t, err)

	return record.ID
}

// answered commits one accepted request and its final answer, returning both message ids.
func answered(t *testing.T, sessions *sessionstore.Store, id int64, ask, answer string) (int64, int64) {
	t.Helper()
	ctx := t.Context()
	in, err := sessions.Enqueue(ctx, sessionstore.Input{
		SessionID: id, Source: sessionstore.InputSourceUser,
		Content: ask,
	})
	require.NoError(t, err)
	result, err := sessions.Commit(ctx, sessionstore.Commit{
		SessionID: id,
		Accept: []sessionstore.Accept{
			{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: ask, LinkRef: -1},
		},
		Messages: []*transcript.Message{{Role: "assistant", Content: answer, FinishType: "stop"}},
	})
	require.NoError(t, err)
	require.Len(t, result.MessageIDs, 2)

	return result.MessageIDs[0], result.MessageIDs[1]
}

func lastMessage(t *testing.T, db *sql.DB, session int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT MAX(id) FROM messages WHERE session_id = ?`,
		session).Scan(&id))

	return id
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func hitRefs(hits []Hit) []string {
	refs := make([]string, 0, len(hits))
	for _, hit := range hits {
		refs = append(refs, hit.Ref)
	}

	return refs
}

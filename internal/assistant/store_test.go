package assistant

import (
	"database/sql"
	"errors"
	"os"
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
	db, sessions, s, root := assistantFixture(t)
	input, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "hello"},
	)
	require.NoError(t, err)
	at := time.Now().UTC()
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, recordSendTx(ctx, tx,
			send{inputID: input.Input.ID, root: root, sessionID: root, createdAt: at}))

		return RecordCallTx(ctx, tx, Call{
			SessionID: root, InputID: input.Input.ID, Number: 1, Phase: phaseMain,
			Request: "desk", Response: "raw", Outcome: "ok", CreatedAt: at,
		})
	}))
	sends, err := s.unobservedSends(ctx, root)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	assert.Equal(t, at, sends[0].createdAt)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		won, err := markObservedTx(ctx, tx, input.Input.ID, at)
		require.NoError(t, err)
		assert.True(t, won)
		won, err = markObservedTx(ctx, tx, input.Input.ID, at.Add(time.Second))
		require.NoError(t, err)
		assert.False(t, won)

		return nil
	}))
	awaited, err := s.AwaitedSessions(ctx)
	require.NoError(t, err)
	assert.Empty(t, awaited)
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
	_, sessions, s, root := assistantFixture(t)
	abort := errors.New("abort turn")
	err := sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := AppendTapeTx(ctx, tx, root, Line{At: time.Now().UTC(), Kind: sayKind, Text: "say: «потеряно»"})
		require.NoError(t, err)

		return abort
	})
	require.ErrorIs(t, err, abort)
	hits, err := s.Search(ctx, Scope{Root: root, Owner: testOwner}, "потеряно")
	require.NoError(t, err)
	assert.Empty(t, hits)
	lines, err := s.RecentTape(ctx, root, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)
}

func TestAssistantSearchFindsInflectedWordsAndCompactedFinals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, s, root := assistantFixture(t)
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

	hits, err := s.Search(ctx, scope, "кеширован*")
	require.NoError(t, err)
	refs := hitRefs(hits)
	assert.Contains(t, refs, "msg "+itoa(final), "a compacted final is still found")
	assert.Len(t, refs, 3, "the final, the say line and the memory note: %v", refs)
	hits, err = s.Search(ctx, scope, "кешированию")
	require.NoError(t, err)
	assert.Empty(t, hits, "a move line is never indexed")
	hits, err = s.Search(ctx, scope, "кеширование")
	require.NoError(t, err)
	require.Len(t, hits, 1, "no stemming: only the exact form matches")
	assert.Equal(t, "say", hits[0].Kind)
	assert.Contains(t, hits[0].Snippet, "[кеширование]")
	_, err = s.Search(ctx, scope, `" * "`)
	require.ErrorIs(t, err, ErrEmptyQuery)
	hits, err = s.Search(ctx, scope, `NEAR(кеш*) OR "прав`)
	require.NoError(t, err, "model text is never FTS syntax")
	assert.NotEmpty(t, hits)
}

func TestAssistantSearchKeepsToAuthorizedRoots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, s, root := assistantFixture(t)
	own := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	userMade := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	foreign := workSession(t, sessions, map[string]any{"manager_id": "manager-b"})
	child := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	surface := workSession(t, sessions, map[string]any{"manager_id": testOwner, "management_surface": true})
	assistantRoot := root
	killed := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	for _, id := range []int64{own, userMade, foreign, child, surface, assistantRoot, killed} {
		answered(t, sessions, id, "найди секрет", "секрет найден")
	}

	_, err := db.ExecContext(ctx, `UPDATE sessions SET parent_id = ?, root_id = ? WHERE id = ?`, own, own, child)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET killed_at = CURRENT_TIMESTAMP, status = 'killed' WHERE id = ?`,
		killed)
	require.NoError(t, err)

	hits, err := s.Search(ctx, Scope{Root: root, Owner: testOwner}, "секрет")
	require.NoError(t, err)
	sessionsFound := map[int64]bool{}
	for _, hit := range hits {
		sessionsFound[hit.Session] = true
	}
	assert.Equal(t, map[int64]bool{own: true, userMade: true}, sessionsFound)
	assert.Len(t, hits, 4, "the accepted request and the final of each root")

	for _, id := range []int64{foreign, child, surface, assistantRoot, killed} {
		require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
			ok, err := authorizedTx(ctx, tx, testOwner, id)
			require.NoError(t, err)
			assert.False(t, ok, id)

			return nil
		}))
	}

	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := authorizedTx(ctx, tx, testOwner, userMade)
		require.NoError(t, err)
		assert.True(t, ok, "a root the user created is addressable too")

		return nil
	}))
	_, err = s.Around(ctx, Scope{Root: root, Owner: testOwner}, "msg "+itoa(lastMessage(t, db, foreign)), 2)
	require.ErrorContains(t, err, "not a message of a session you can see")
}

func TestAssistantSearchSkipsNoiseAndRejectedRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, s, root := assistantFixture(t)
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
	hits, err := s.Search(ctx, scope, "служебн*")
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
	db, sessions, s, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	messages := make([]*transcript.Message, 0, indexBatchSize+20)
	for range indexBatchSize + 20 {
		messages = append(messages, &transcript.Message{Role: "assistant", Content: "пакет", FinishType: "stop"})
	}
	_, err := sessions.Commit(ctx, sessionstore.Commit{SessionID: work, Messages: messages})
	require.NoError(t, err)
	hits, err := s.Search(ctx, Scope{Root: root, Owner: testOwner}, "пакет")
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
	_, sessions, s, root := assistantFixture(t)
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
	desk, err := s.LoadDesk(ctx, root, 0, now)
	require.NoError(t, err)
	require.Len(t, desk.Memory, 1)
	assert.Equal(t, permanent, desk.Memory[0].ID)
	all, err := s.Memory(ctx, root)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	scope := Scope{Root: root, Owner: testOwner}
	hits, err := s.Search(ctx, scope, "отчёт")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"m" + itoa(expired), "m" + itoa(permanent)}, hitRefs(hits))
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		return deleteMemoryTx(ctx, tx, root, permanent)
	}))
	hits, err = s.Search(ctx, scope, "отчёт")
	require.NoError(t, err)
	assert.Equal(t, []string{"m" + itoa(expired)}, hitRefs(hits), "a deleted note is never found again")
}

func TestAssistantAroundShowsNeighbours(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, s, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	answered(t, sessions, work, "первый вопрос", "первый ответ")
	_, middle := answered(t, sessions, work, "второй вопрос", "второй ответ")
	answered(t, sessions, work, "третий вопрос", "третий ответ")
	scope := Scope{Root: root, Owner: testOwner}
	text, err := s.Around(ctx, scope, "msg "+itoa(middle), 1)
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
	text, err = s.Around(ctx, scope, "line "+itoa(ids[1]), 1)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(text, "line "))
	_, err = s.Around(ctx, scope, "line 999", 1)
	require.ErrorContains(t, err, "not on your tape")
	_, err = s.Around(ctx, scope, "m12", 1)
	require.ErrorContains(t, err, "use \"msg <id>\"")
}

func TestAssistantWorkRoots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, s, root := assistantFixture(t)
	touched := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	userMade := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	answered(t, sessions, userMade, strings.Repeat("длинный вопрос ", 20), "ответ")
	in, err := sessions.Enqueue(ctx, sessionstore.Input{
		SessionID: touched, Source: sessionstore.InputSourceUser,
		Content: "go",
	})
	require.NoError(t, err)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, recordSendTx(ctx, tx,
			send{inputID: in.Input.ID, root: root, sessionID: touched, createdAt: time.Now().UTC()}))

		return saveAboutTx(ctx, tx, touched, "why it exists", time.Now().UTC())
	}))
	scope := Scope{Root: root, Owner: testOwner}
	roots, _, err := s.Recent(ctx, scope, 100)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, touched, roots[0].ID)
	assert.True(t, roots[0].Pending)
	assert.Equal(t, "why it exists", roots[0].About)
	roots, err = s.cards(ctx, scope, 0)
	require.NoError(t, err)
	require.Len(t, roots, 2)
	for _, work := range roots {
		if work.ID == userMade {
			assert.False(t, work.touched)
			assert.Len(t, []rune(work.About), aboutFallbackChars)
			assert.True(t, strings.HasPrefix(work.About, "длинный вопрос длинный"))
		}
	}
}

func TestAssistantStopSuppressesStaleObservation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, s, root := assistantFixture(t)
	work := workSession(t, sessions, map[string]any{"manager_id": testOwner})
	host := &loopHost{sessions: sessions}
	require.NoError(t, s.Send(ctx, host, Scope{Root: root, Owner: testOwner}, work, "complete while stopping"))
	var pending int64
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT id FROM session_inbox WHERE session_id = ? AND state = 'pending'`,
			work).Scan(&pending),
	)
	_, err := sessions.Commit(ctx, sessionstore.Commit{
		SessionID: work,
		Accept: []sessionstore.Accept{
			{InputID: pending, State: sessionstore.InputStateAccepted, Content: "complete while stopping", LinkRef: -1},
		},
		Messages: []*transcript.Message{{Role: "assistant", Content: "done", FinishType: "stop"}},
	})
	require.NoError(t, err)
	sends, err := s.unobservedSends(ctx, work)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	require.NoError(t, s.settleSends(ctx, work))
	require.NoError(t, s.observeSend(ctx, sends[0], "devbox", false, nil))
	var events int
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ? AND source = 'agent'`,
			root).Scan(&events),
	)
	assert.Zero(t, events, "an observation that listed the send before the stop settled it emits nothing")
}

func TestAssistantReadKeepsNewestWhole(t *testing.T) {
	t.Parallel()
	old, middle, newest := strings.Repeat("a", 30), strings.Repeat("b", 30), strings.Repeat("c", 30)
	assert.Equal(t, middle+"\n\n"+newest, newestWithin([]string{old, middle, newest}, 70))
	assert.Equal(t, "cccccc\n[…]\nccc", newestWithin([]string{newest}, 14), "only a lone oversized entry is cut")
}

func assistantFixture(t *testing.T) (*sql.DB, *sessionstore.Store, *Store, int64) {
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
	root, err := sessions.CreateSession(ctx, project, "model", "", map[string]any{"manager_id": testOwner})
	require.NoError(t, err)
	_, err = db.ExecContext(
		ctx,
		`UPDATE sessions SET agent_type = ? WHERE id = ?`,
		sessionstore.AssistantAgentType,
		root.ID,
	)
	require.NoError(t, err)

	return db, sessions, New(db, sessions, t.TempDir()), root.ID
}

// devboxWork seeds a completed work root in its own devbox project, answered once to an observed send.
func devboxWork(t *testing.T, db *sql.DB, sessions *sessionstore.Store, root int64) int64 {
	t.Helper()
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "devbox")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	project, err := sessions.GetOrCreateProject(ctx, dir)
	require.NoError(t, err)
	record, err := sessions.CreateSession(ctx, project, "model", "", map[string]any{"manager_id": testOwner})
	require.NoError(t, err)
	answered(t, sessions, record.ID, "build", "tail ok")
	require.NoError(t, sessions.UpdateSessionStatus(ctx, record.ID, sessionstore.SessionStatusCompleted))
	now := time.Now().UTC()
	_, err = db.ExecContext(ctx, `INSERT INTO assistant_sends
		(input_id, assistant_session_id, session_id, created_at, observed_at)
		SELECT id, ?, session_id, ?, ? FROM session_inbox WHERE session_id = ?`,
		root, now, now, record.ID)
	require.NoError(t, err)

	return record.ID
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

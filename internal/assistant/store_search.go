package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	searchMessage = iota
	_
	searchMemory
	searchFocus
	searchKinds
)

const (
	searchHits      = 10
	indexBatchSize  = 500
	aroundTextChars = 2000
	// indexRootSQL skips children and assistant roots over `sessions s`.
	indexRootSQL = `COALESCE(s.parent_id, 0) = 0 AND COALESCE(s.agent_type, '') <> '` + sessionstore.AssistantAgentType + `'`
	// requestSQL identifies accepted work requests over messages aliased as m.
	requestSQL = `(m.role = 'user' AND EXISTS (SELECT 1 FROM session_inbox i WHERE i.session_id = m.session_id
		AND i.state = 'accepted' AND i.accepted_message_id IS NOT NULL
		AND i.accepted_message_id = m.id AND i.source = 'user'))`
	indexableSQL    = `m.rejected_reason IS NULL AND (` + requestSQL + ` OR ` + sessionstore.FinalMessageSQL + `)`
	ownIndexableSQL = `m.rejected_reason IS NULL AND (` + requestSQL + ` OR (m.role = 'assistant'
		AND TRIM(COALESCE(m.content, ''), ' ' || char(9, 10, 13)) <> ''))`
)

// ErrEmptyQuery reports a query with no searchable word.
var ErrEmptyQuery = errors.New("the query has no words to search for")

// Hit is one search result; Ref is what around, rewrite_memory or update_focus accept.
type Hit struct {
	Ref     string
	Session int64
	Project string
	Kind    string
	At      time.Time
	Snippet string
}

// Search catches the message index up before matching, so it never misses a committed final.
func (s *Store) Search(ctx context.Context, scope Scope, query string) ([]Hit, error) {
	match, err := matchQuery(query)
	if err != nil {
		return nil, err
	}

	if err := s.catchUp(ctx); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT assistant_search.rowid,
			snippet(assistant_search, 0, '[', ']', '…', 24)
		FROM assistant_search
		WHERE assistant_search MATCH ? AND (
			(assistant_search.rowid % 4 <> 0 AND assistant_search.scope = ?)
			OR (assistant_search.rowid % 4 = 0 AND EXISTS (
				SELECT 1 FROM messages m JOIN sessions s ON s.id = m.session_id
				WHERE m.id = assistant_search.rowid / 4 AND m.rejected_reason IS NULL
				AND (`+workRootSQL+` OR m.session_id = ?)))
		)
		ORDER BY bm25(assistant_search) LIMIT ?`, match, scope.Root, scope.Owner, scope.Root, searchHits)
	if err != nil {
		return nil, fmt.Errorf("search assistant history: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	var rowids []int64

	for rows.Next() {
		var hit Hit
		var rowid int64

		if err := rows.Scan(&rowid, &hit.Snippet); err != nil {
			return nil, fmt.Errorf("scan assistant search hit: %w", err)
		}

		hits, rowids = append(hits, hit), append(rowids, rowid)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant search hits: %w", err)
	}

	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close assistant search hits: %w", err)
	}

	described := hits[:0]

	for i, rowid := range rowids {
		err := s.describeHit(ctx, &hits[i], rowid/searchKinds, int(rowid%searchKinds))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}

		if err != nil {
			return nil, err
		}

		described = append(described, hits[i])
	}

	return described, nil
}

// Around shows a message among its session's requests and replies.
func (s *Store) Around(ctx context.Context, scope Scope, ref string, count int) (string, error) {
	kind, raw, _ := strings.Cut(strings.TrimSpace(ref), " ")

	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 || kind != "msg" {
		return "", fmt.Errorf("ref %q: use \"msg <id>\" from a search result", ref)
	}

	return s.aroundMessage(ctx, scope, id, count)
}

func (s *Store) aroundMessage(ctx context.Context, scope Scope, id int64, count int) (string, error) {
	var session int64
	var project string

	err := s.db.QueryRowContext(ctx, `SELECT m.session_id, COALESCE(p.name, '')
		FROM messages m JOIN sessions s ON s.id = m.session_id LEFT JOIN projects p ON p.id = s.project_id
		WHERE m.id = ? AND ((m.session_id = ? AND `+ownIndexableSQL+`)
		OR (`+indexableSQL+` AND `+workRootSQL+`))`, id, scope.Root, scope.Owner).Scan(&session, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("msg %d is not a message of a session you can see", id)
	}

	if err != nil {
		return "", fmt.Errorf("locate assistant message %d: %w", id, err)
	}

	predicate := indexableSQL
	if session == scope.Root {
		predicate = ownIndexableSQL
	}

	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.role, COALESCE(m.content, ''), m.created_at
		FROM messages m WHERE m.session_id = ? AND `+predicate+` AND (m.id = ?
			OR m.id IN (SELECT m.id FROM messages m WHERE m.session_id = ? AND m.id < ? AND `+predicate+`
				ORDER BY m.id DESC LIMIT ?)
			OR m.id IN (SELECT m.id FROM messages m WHERE m.session_id = ? AND m.id > ? AND `+predicate+`
				ORDER BY m.id LIMIT ?))
		ORDER BY m.id`, session, id, session, id, count, session, id, count)
	if err != nil {
		return "", fmt.Errorf("load messages around %d: %w", id, err)
	}
	defer rows.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "around msg %d in %d %s:\n", id, session, project)

	for rows.Next() {
		var at time.Time
		var other int64
		var role, text string

		if err := rows.Scan(&other, &role, &text, &at); err != nil {
			return "", fmt.Errorf("scan message around %d: %w", id, err)
		}

		fmt.Fprintf(&b, "%smsg %d · %s · %s: %s\n", aroundMarker(other == id), other, at.UTC().Format(toolTime),
			role, sessionstore.ClipMiddle(strings.TrimSpace(text), aroundTextChars))
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read messages around %d: %w", id, err)
	}

	return b.String(), nil
}

func (s *Store) describeHit(ctx context.Context, hit *Hit, id int64, kind int) error {
	var err error

	switch kind {
	case searchMessage:
		hit.Ref = fmt.Sprintf("msg %d", id)
		err = s.db.QueryRowContext(ctx, `SELECT m.session_id, m.role, m.created_at, COALESCE(p.name, '')
			FROM messages m JOIN sessions s ON s.id = m.session_id LEFT JOIN projects p ON p.id = s.project_id
			WHERE m.id = ?`, id).Scan(&hit.Session, &hit.Kind, &hit.At, &hit.Project)
	case searchMemory:
		hit.Ref = fmt.Sprintf("m%d", id)
		err = s.db.QueryRowContext(ctx, `SELECT COALESCE('until ' || until, 'permanent'), updated_at
			FROM assistant_memory WHERE id = ?`, id).Scan(&hit.Kind, &hit.At)
	default:
		hit.Ref = fmt.Sprintf("f%d", id)
		hit.Kind = "focus"
		err = s.db.QueryRowContext(ctx, `SELECT updated_at FROM assistant_focus WHERE id = ?`, id).Scan(&hit.At)
	}

	if err != nil {
		return fmt.Errorf("describe assistant search hit %s: %w", hit.Ref, err)
	}

	return nil
}

func (s *Store) catchUp(ctx context.Context) error {
	for {
		indexed, err := s.indexBatch(ctx)
		if err != nil {
			return err
		}

		if indexed < indexBatchSize {
			return nil
		}
	}
}

// indexBatch reads the watermark inside its write transaction, because every root shares it.
func (s *Store) indexBatch(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin assistant index batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var from, to int64
	var count int

	if err := tx.QueryRowContext(ctx, `SELECT message_id FROM assistant_search_state WHERE id = 1`).
		Scan(&from); err != nil {
		return 0, fmt.Errorf("read assistant index watermark: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), ?) FROM (
		SELECT id FROM messages WHERE id > ? ORDER BY id LIMIT ?)`, from, from, indexBatchSize).
		Scan(&count, &to); err != nil {
		return 0, fmt.Errorf("select assistant index batch: %w", err)
	}

	if count == 0 {
		return 0, nil
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO assistant_search (rowid, body, scope)
		SELECT m.id * 4, m.content, m.session_id FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE m.id > ? AND m.id <= ? AND ((`+indexRootSQL+` AND `+indexableSQL+`)
		OR (COALESCE(s.parent_id, 0) = 0 AND s.agent_type = ?
		AND `+ownIndexableSQL+`))`, from, to, sessionstore.AssistantAgentType); err != nil {
		return 0, fmt.Errorf("index assistant messages: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE assistant_search_state SET message_id = ? WHERE id = 1`,
		to); err != nil {
		return 0, fmt.Errorf("advance assistant index watermark: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit assistant index batch: %w", err)
	}

	return count, nil
}

// matchQuery quotes every word so model text is never parsed as FTS syntax; a trailing * stays a prefix.
func matchQuery(query string) (string, error) {
	var terms []string

	for field := range strings.FieldsSeq(query) {
		prefix := strings.HasSuffix(field, "*")

		word := strings.Map(func(r rune) rune {
			if r == '"' || unicode.IsControl(r) {
				return -1
			}

			return r
		}, strings.Trim(field, "*"))
		if strings.IndexFunc(word, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
			continue
		}

		term := `"` + word + `"`
		if prefix {
			term += "*"
		}

		terms = append(terms, term)
	}

	if len(terms) == 0 {
		return "", ErrEmptyQuery
	}

	return strings.Join(terms, " OR "), nil
}

func indexTx(ctx context.Context, tx *sql.Tx, kind int, id, scope int64, body string) error {
	if err := unindexTx(ctx, tx, kind, id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO assistant_search (rowid, body, scope) VALUES (?, ?, ?)`,
		id*searchKinds+int64(kind), body, scope); err != nil {
		return fmt.Errorf("index assistant search row: %w", err)
	}

	return nil
}

func unindexTx(ctx context.Context, tx *sql.Tx, kind int, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assistant_search WHERE rowid = ?`,
		id*searchKinds+int64(kind)); err != nil {
		return fmt.Errorf("unindex assistant search row: %w", err)
	}

	return nil
}

func aroundMarker(target bool) string {
	if target {
		return "> "
	}

	return "  "
}

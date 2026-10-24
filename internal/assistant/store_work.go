package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	// workRootSQL is the single authority predicate over `sessions s`; its one placeholder binds the owner.
	workRootSQL = `json_extract(s.attributes, '$.manager_id') = ? AND COALESCE(s.parent_id, 0) = 0
		AND COALESCE(s.agent_type, '') <> '` + sessionstore.AssistantAgentType + `'
		AND s.killed_at IS NULL AND COALESCE(s.status, '') <> 'killed'
		AND (COALESCE(json_type(s.attributes, '$.management_surface'), 'null') IN ('null', 'false')
			OR (json_type(s.attributes, '$.management_surface') = 'text'
				AND json_extract(s.attributes, '$.management_surface') IN ('', 'false', '0')))`
	aboutFallbackChars = 120
	// Below the event limit, so a read is never cut in the middle by the desk.
	readBudget    = 18000
	workingStatus = "working"
	failedStatus  = "failed"
)

// Recent lists the work roots this assistant sent to, most active first, and how many there are.
func (s *Store) Recent(ctx context.Context, scope Scope, limit int) ([]Card, int, error) {
	cards, err := s.cards(ctx, scope, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("list recent assistant work: %w", err)
	}

	cards = slices.DeleteFunc(cards, func(card Card) bool { return !card.touched })

	return cards[:min(limit, len(cards))], len(cards), nil
}

// List pages every addressable work root, optionally of one project key.
func (s *Store) List(ctx context.Context, scope Scope, offset, limit int, project string) ([]Card, int, error) {
	cards, err := s.cards(ctx, scope, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("list assistant work: %w", err)
	}

	if project != "" {
		id, err := s.projectID(ctx, project)
		if err != nil {
			return nil, 0, err
		}

		cards = slices.DeleteFunc(cards, func(card Card) bool { return card.projectID != id })
	}

	offset = min(offset, len(cards))

	return cards[offset:min(offset+limit, len(cards))], len(cards), nil
}

// Target authorizes one session with the same predicate as listings and search.
func (s *Store) Target(ctx context.Context, scope Scope, id int64) (Card, error) {
	cards, err := s.cards(ctx, scope, id)
	if err != nil {
		return Card{}, err
	}

	if len(cards) == 0 {
		return Card{}, fmt.Errorf("session %d is not a work session you can address", id)
	}

	return cards[0], nil
}

// Read includes compacted messages: an old final is still what the session answered.
func (s *Store) Read(ctx context.Context, id int64, count int) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.role, COALESCE(m.content, '') FROM messages m
		WHERE m.session_id = ? AND `+indexableSQL+` ORDER BY m.id`, id)
	if err != nil {
		return "", fmt.Errorf("load assistant read transcript: %w", err)
	}
	defer rows.Close()

	var entries []string

	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return "", fmt.Errorf("load assistant read transcript: %w", err)
		}

		entries = append(entries, role+": "+strings.TrimSpace(content))
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("load assistant read transcript: %w", err)
	}

	if len(entries) == 0 {
		return "(no requests or answers yet)", nil
	}

	return newestWithin(entries[max(0, len(entries)-count):], readBudget), nil
}

// Open creates the work root, then sends its first instruction with the about line.
func (s *Store) Open(ctx context.Context, host Host, scope Scope, project, text, about string) (Card, error) {
	choice, err := s.resolveProject(ctx, project)
	if err != nil {
		return Card{}, err
	}

	id, err := host.CreateWork(ctx, scope.Owner, choice.id, choice.name, choice.path)
	if err != nil {
		return Card{}, fmt.Errorf("open assistant work: %w", err)
	}

	if err := s.deliver(ctx, host, scope, id, text, about); err != nil {
		return Card{}, err
	}

	now := time.Now().UTC()

	return Card{ID: id, Project: choice.name, Status: workingStatus, CreatedAt: now, Activity: now, About: about}, nil
}

// Send re-checks authority inside the delivery transaction.
func (s *Store) Send(ctx context.Context, host Host, scope Scope, id int64, text string) error {
	return s.deliver(ctx, host, scope, id, text, "")
}

// Stop settles the session's sends before the stop command, so the stopped run's exit finds nothing to report.
func (s *Store) Stop(ctx context.Context, host Host, scope Scope, id int64) error {
	if _, err := s.Target(ctx, scope, id); err != nil {
		return err
	}

	if err := s.settleSends(ctx, id); err != nil {
		return err
	}

	if err := host.Stop(ctx, id); err != nil {
		return fmt.Errorf("stop assistant work: %w", err)
	}

	return nil
}

func (s *Store) cards(ctx context.Context, scope Scope, id int64) ([]Card, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.project_id, COALESCE(p.name, ''), s.status,
			s.created_at, s.updated_at, COALESCE(s.context_baseline_prompt_tokens, 0),
			EXISTS(SELECT 1 FROM assistant_sends a
				WHERE a.session_id = s.id AND a.assistant_session_id = ? AND a.observed_at IS NULL),
			EXISTS(SELECT 1 FROM assistant_sends a WHERE a.session_id = s.id AND a.assistant_session_id = ?),
			o.created_at, w.about, i.raw_content
		FROM sessions s
		LEFT JOIN projects p ON p.id = s.project_id
		LEFT JOIN session_outbox o ON o.id = (SELECT id FROM session_outbox WHERE session_id = s.id
			AND type = 'message_persistent' AND source_key LIKE 'message:%' ORDER BY id DESC LIMIT 1)
		LEFT JOIN assistant_work w ON w.session_id = s.id
		LEFT JOIN session_inbox i ON i.id = (SELECT id FROM session_inbox WHERE session_id = s.id
			AND source = 'user' AND state = 'accepted' ORDER BY id LIMIT 1)
		WHERE `+workRootSQL+` AND (? = 0 OR s.id = ?)`, scope.Root, scope.Root, scope.Owner, id, id)
	if err != nil {
		return nil, fmt.Errorf("load assistant work roots: %w", err)
	}
	defer rows.Close()

	var cards []Card

	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return nil, err
		}

		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant work roots: %w", err)
	}

	sortCards(cards)

	return cards, nil
}

func scanCard(rows *sql.Rows) (Card, error) {
	var (
		card             Card
		status           sql.NullString
		created, updated sql.NullTime
		final            sql.NullTime
		about, asked     sql.NullString
	)

	if err := rows.Scan(&card.ID, &card.projectID, &card.Project, &status, &created, &updated,
		&card.ContextTokens, &card.Pending, &card.touched, &final, &about, &asked); err != nil {
		return Card{}, fmt.Errorf("scan assistant work root: %w", err)
	}

	card.Status = cardStatus(sessionstore.SessionStatus(status.String))
	card.CreatedAt, card.Activity = created.Time, updated.Time

	if final.Time.After(card.Activity) {
		card.Activity = final.Time
	}

	card.About = about.String
	if !about.Valid {
		card.About = Clip(strings.Join(strings.Fields(asked.String), " "), aboutFallbackChars)
	}

	return card, nil
}

func sortCards(cards []Card) {
	slices.SortStableFunc(cards, func(a, b Card) int {
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		return cmp.Compare(b.ID, a.ID)
	})
}

func cardStatus(status sessionstore.SessionStatus) string {
	switch status {
	case sessionstore.SessionStatusActive, sessionstore.SessionStatusSuspended:
		return workingStatus
	case sessionstore.SessionStatusCompleted:
		return "done"
	case sessionstore.SessionStatusError:
		return failedStatus
	case sessionstore.SessionStatusStopping, sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusTerminating, sessionstore.SessionStatusKilled:
		return string(sessionstore.SessionStatusStopped)
	default:
		return string(sessionstore.SessionStatusStopped)
	}
}

func authorizedTx(ctx context.Context, tx *sql.Tx, owner string, id int64) (bool, error) {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s WHERE s.id = ? AND `+workRootSQL+`)`,
		id, owner).Scan(&ok); err != nil {
		return false, fmt.Errorf("authorize assistant target %d: %w", id, err)
	}

	return ok, nil
}

func (s *Store) deliver(ctx context.Context, host Host, scope Scope, id int64, text, about string) error {
	err := host.Deliver(ctx, id, text, func(tx *sql.Tx) error {
		return s.instructTx(ctx, tx, scope, id, text, about)
	})
	if err != nil {
		return fmt.Errorf("deliver assistant instruction: %w", err)
	}

	return nil
}

func (s *Store) instructTx(ctx context.Context, tx *sql.Tx, scope Scope, id int64, text, about string) error {
	ok, err := authorizedTx(ctx, tx, scope.Owner, id)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("session %d is not a work session you can address", id)
	}

	in, err := sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
		SessionID:  id,
		Source:     sessionstore.InputSourceUser,
		Content:    text,
		Attributes: map[string]any{"from": "assistant"},
	})
	if err != nil {
		return fmt.Errorf("enqueue assistant instruction: %w", err)
	}

	now := time.Now().UTC()

	err = recordSendTx(ctx, tx, send{inputID: in.Input.ID, root: scope.Root, sessionID: id, createdAt: now})
	if err != nil {
		return err
	}

	if about != "" {
		return saveAboutTx(ctx, tx, id, about, now)
	}

	return nil
}

// newestWithin keeps whole entries from the newest backwards; only a single oversized
// entry is cut, and then in its middle.
func newestWithin(entries []string, budget int) string {
	var kept []string

	used := 0

	for _, entry := range slices.Backward(entries) {
		size := utf8.RuneCountInString(entry) + 2
		if used+size > budget {
			if len(kept) == 0 {
				kept = append(kept, ClipMiddle(entry, budget))
			}

			break
		}

		kept = append(kept, entry)
		used += size
	}

	slices.Reverse(kept)

	return strings.Join(kept, "\n\n")
}

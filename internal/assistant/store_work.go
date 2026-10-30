package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	// workRootSQL is the single authority predicate over `sessions s`; its one placeholder binds the owner.
	workRootSQL = `json_extract(s.attributes, '$.manager_id') = ? AND COALESCE(s.parent_id, 0) = 0
		AND COALESCE(s.agent_type, '') <> '` + sessionstore.AssistantAgentType + `'
		AND s.killed_at IS NULL AND COALESCE(s.status, '') <> 'killed'`
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

func (s *Store) cards(ctx context.Context, scope Scope, id int64) ([]Card, error) {
	const query = `SELECT s.id, s.project_id, COALESCE(p.name, ''), s.status,
			s.created_at, s.updated_at, COALESCE(s.context_baseline_prompt_tokens, 0),
			(s.status NOT IN ('stopped', 'stopping', 'terminating', 'killed', 'error') AND
			 EXISTS(SELECT 1 FROM session_inbox i WHERE i.session_id = s.id
			 AND json_extract(i.attributes, '$.observer_session_id') = ? AND ` + sessionstore.ObserverOpenInputSQL + `)),
			EXISTS(SELECT 1 FROM session_inbox i WHERE i.session_id = s.id
			 AND json_extract(i.attributes, '$.observer_session_id') = ?),
			o.created_at, json_extract(s.attributes, '$.title'), i.raw_content,
			s.model, s.reasoning_level, b.state, b.armed_at, b.baseline_cost_usd,
			b.cost_limit_usd, b.duration_seconds, b.fired_reason, b.park_phase,
			CASE WHEN b.state = 'armed' AND b.cost_limit_usd IS NOT NULL THEN
			 (SELECT COALESCE(SUM(m.cost_usd), 0) FROM sessions tree
			 LEFT JOIN messages m ON m.session_id = tree.id WHERE tree.id = s.id OR tree.root_id = s.id) END,
			EXISTS (SELECT 1 FROM schedules WHERE schedules.session_id = s.id AND NOT (` + sessionstore.SleepScheduleSQL + `))
		FROM sessions s
		LEFT JOIN projects p ON p.id = s.project_id
		LEFT JOIN session_budgets b ON b.root_session_id = s.id
		LEFT JOIN session_outbox o ON o.id = (SELECT id FROM session_outbox WHERE session_id = s.id
			AND type = 'message' AND source_key LIKE 'message:%' ORDER BY id DESC LIMIT 1)
		LEFT JOIN session_inbox i ON i.id = (SELECT id FROM session_inbox WHERE session_id = s.id
			AND source = 'user' AND state = 'accepted' ORDER BY id LIMIT 1)
		WHERE ` + workRootSQL + ` AND (? = 0 OR s.id = ?)`

	rows, err := s.db.QueryContext(ctx, query, scope.Root, scope.Root, scope.Owner, id, id)
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
		card                 Card
		status               sql.NullString
		created, updated     sql.NullTime
		final                sql.NullTime
		title, asked         sql.NullString
		state, reason, phase sql.NullString
		armed                sql.NullTime
		baseline             sql.NullFloat64
		record               budget.Record
	)

	if err := rows.Scan(&card.ID, &card.projectID, &card.Project, &status, &created, &updated,
		&card.ContextTokens, &card.Pending, &card.touched, &final, &title, &asked,
		&card.Model, &card.Effort, &state, &armed, &baseline, &record.CostLimitUSD,
		&record.DurationSeconds, &reason, &phase, &card.TreeCostUSD, &card.Scheduled); err != nil {
		return Card{}, fmt.Errorf("scan assistant work root: %w", err)
	}

	if state.Valid {
		record.RootSessionID, record.State, record.ArmedAt = card.ID, budget.State(state.String), armed.Time
		record.BaselineCostUSD, record.FiredReason, record.ParkPhase = baseline.Float64, reason.String, phase.String
		card.Budget = &record
	}

	card.Status = cardStatus(sessionstore.SessionStatus(status.String))
	card.CreatedAt, card.Activity = created.Time, updated.Time

	if final.Time.After(card.Activity) {
		card.Activity = final.Time
	}

	card.Title = title.String
	if !title.Valid {
		card.Title = Clip(strings.Join(strings.Fields(asked.String), " "), aboutFallbackChars)
	}

	return card, nil
}

func sortCards(cards []Card) {
	slices.SortStableFunc(cards, func(a, b Card) int {
		return cmp.Or(b.Activity.Compare(a.Activity), cmp.Compare(b.ID, a.ID))
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
	}

	return string(sessionstore.SessionStatusStopped)
}

func authorizedTx(ctx context.Context, tx *sql.Tx, owner string, id int64) (bool, error) {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s WHERE s.id = ? AND `+workRootSQL+`)`,
		id, owner).Scan(&ok); err != nil {
		return false, fmt.Errorf("authorize assistant target %d: %w", id, err)
	}

	return ok, nil
}

func (s *Store) instructTx(ctx context.Context, tx *sql.Tx, scope Scope,
	id int64, text string, attributes map[string]any,
) (*sessionstore.InboxInput, error) {
	ok, err := authorizedTx(ctx, tx, scope.Owner, id)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, fmt.Errorf("session %d is not a work session you can address", id)
	}

	if attributes == nil {
		attributes = make(map[string]any)
	}

	attributes["from"] = "assistant"
	attributes[sessionstore.ObserverSessionIDAttribute] = scope.Root

	result, err := sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
		SessionID:  id,
		Source:     sessionstore.InputSourceUser,
		Content:    text,
		Attributes: attributes,
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue assistant instruction: %w", err)
	}

	return result.Input, nil
}

// newestWithin keeps whole entries from the newest backwards; only a single oversized
// entry is cut, and then in its middle.
func newestWithin(entries []string, limit int) string {
	used := 0

	for i := range slices.Backward(entries) {
		if used += utf8.RuneCountInString(entries[i]) + 2; used > limit {
			if i == len(entries)-1 {
				return sessionstore.ClipMiddle(entries[i], limit)
			}

			return strings.Join(entries[i+1:], "\n\n")
		}
	}

	return strings.Join(entries, "\n\n")
}

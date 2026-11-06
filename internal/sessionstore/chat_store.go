package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const liveChatRootSQL = `s.parent_id = 0 AND s.killed_at IS NULL
	AND s.status NOT IN ('terminating', 'killed')
	AND json_extract(s.attributes, '$.manager_id') = ?`

// ChatOutput includes non-rendering lifecycle rows that still advance the chat cursor.
type ChatOutput struct {
	ID        int64
	SessionID int64
	Name      string
	Type      OutputType
	Content   string
}

// ChatSession describes a live manager-owned root.
type ChatSession struct {
	ID        int64
	Name      string
	Scheduled bool
}

// BindChat preserves the user's selection across manager restarts.
func (s *Store) BindChat(ctx context.Context, managerID, chatID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_focus (manager_id, chat_id, session_id, last_sent_id)
		VALUES (?, ?, NULL, NULL) ON CONFLICT(manager_id, chat_id) DO NOTHING`, managerID, chatID)
	if err != nil {
		return fmt.Errorf("bind chat: %w", err)
	}

	return nil
}

// ChatFocus returns zero when no session is selected.
func (s *Store) ChatFocus(ctx context.Context, managerID, chatID string) (int64, error) {
	var sessionID sql.NullInt64

	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM chat_focus
		WHERE manager_id = ? AND chat_id = ?`, managerID, chatID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("load chat focus: %w", err)
	}

	return sessionID.Int64, nil
}

// SetChatFocus validates the root in the same statement that changes the chat binding.
func (s *Store) SetChatFocus(ctx context.Context, managerID, chatID string, sessionID int64) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO chat_focus (manager_id, chat_id, session_id, last_sent_id)
		SELECT ?, ?, s.id, NULL FROM sessions s WHERE s.id = ? AND `+liveChatRootSQL+`
		ON CONFLICT(manager_id, chat_id) DO UPDATE SET session_id = excluded.session_id, last_sent_id = NULL`,
		managerID, chatID, sessionID, managerID)
	if err != nil {
		return fmt.Errorf("set chat focus: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count chat focus update: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("#%d is not a live session", sessionID)
	}

	return nil
}

// ResetChatFocus leaves a newer selection untouched.
func (s *Store) ResetChatFocus(ctx context.Context, managerID, chatID string, from int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_focus SET session_id = NULL, last_sent_id = NULL
		WHERE manager_id = ? AND chat_id = ? AND session_id = ?`, managerID, chatID, from)
	if err != nil {
		return false, fmt.Errorf("reset chat focus: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count chat focus reset: %w", err)
	}

	return affected == 1, nil
}

// ChatOutputs includes lifecycle rows so delivery can consume their positions in the log.
func (s *Store) ChatOutputs(
	ctx context.Context,
	sessionID, after int64,
	limit int,
) ([]ChatOutput, error) {
	if limit <= 0 {
		return nil, errors.New("chat output limit must be positive")
	}

	name, err := s.chatOutputName(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, type, content FROM session_outbox
		WHERE session_id = ? AND id > ? ORDER BY id LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("load chat outputs: %w", err)
	}
	defer rows.Close()

	var outputs []ChatOutput

	for rows.Next() {
		var output ChatOutput
		if err := rows.Scan(&output.ID, &output.SessionID, &output.Type, &output.Content); err != nil {
			return nil, fmt.Errorf("scan chat output: %w", err)
		}

		output.Name = name

		outputs = append(outputs, output)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat outputs: %w", err)
	}

	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close chat outputs: %w", err)
	}

	return outputs, nil
}

// ChatCursor reads the focus and its entry obligation in one snapshot.
func (s *Store) ChatCursor(ctx context.Context, managerID, chatID string) (int64, int64, bool, error) {
	var session, cursor sql.NullInt64

	err := s.db.QueryRowContext(ctx, `SELECT session_id,last_sent_id FROM chat_focus
		WHERE manager_id = ? AND chat_id = ?`, managerID, chatID).Scan(&session, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}

	if err != nil {
		return 0, 0, false, fmt.Errorf("load chat cursor: %w", err)
	}

	return session.Int64, cursor.Int64, cursor.Valid, nil
}

// ChatEntry snapshots the renderable tail and the position beyond all current rows together.
func (s *Store) ChatEntry(ctx context.Context, sessionID int64, limit int) ([]ChatOutput, int64, error) {
	if limit <= 0 {
		return nil, 0, errors.New("chat entry limit must be positive")
	}

	name, err := s.chatOutputName(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx, `WITH newest AS (
		SELECT COALESCE(MAX(id),0) AS id FROM session_outbox WHERE session_id = ?
	), tail AS (
		SELECT id,type,content FROM session_outbox WHERE session_id = ?
			AND type NOT IN ('session_opened','session_replaced') ORDER BY id DESC LIMIT ?
	)
	SELECT newest.id,tail.id,tail.type,tail.content FROM newest LEFT JOIN tail ON TRUE ORDER BY tail.id`,
		sessionID, sessionID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("load chat entry: %w", err)
	}
	defer rows.Close()
	var outputs []ChatOutput
	var newest int64

	for rows.Next() {
		var id sql.NullInt64

		var kind, content sql.NullString
		if err := rows.Scan(&newest, &id, &kind, &content); err != nil {
			return nil, 0, fmt.Errorf("scan chat entry: %w", err)
		}

		if id.Valid {
			outputs = append(outputs, ChatOutput{
				ID: id.Int64, SessionID: sessionID,
				Name: name, Type: OutputType(kind.String), Content: content.String,
			})
		}
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate chat entry: %w", err)
	}

	return outputs, newest, nil
}

// EnterChat acknowledges an entry only while its focus still owes one.
func (s *Store) EnterChat(ctx context.Context, managerID, chatID string, sessionID, newest int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_focus SET last_sent_id = ?
		WHERE manager_id = ? AND chat_id = ? AND session_id = ? AND last_sent_id IS NULL`,
		newest, managerID, chatID, sessionID)
	if err != nil {
		return false, fmt.Errorf("enter chat: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count chat entry: %w", err)
	}

	return affected == 1, nil
}

// AdvanceChat cannot consume an entry requested by a re-switch to the same focus.
func (s *Store) AdvanceChat(ctx context.Context, managerID, chatID string, sessionID, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_focus SET last_sent_id = ?
		WHERE manager_id = ? AND chat_id = ? AND session_id = ?
			AND last_sent_id IS NOT NULL AND last_sent_id < ?`, id, managerID, chatID, sessionID, id)
	if err != nil {
		return false, fmt.Errorf("advance chat: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count chat advance: %w", err)
	}

	return affected == 1, nil
}

func (s *Store) chatOutputName(ctx context.Context, sessionID int64) (string, error) {
	var project, title string

	err := s.db.QueryRowContext(ctx, `SELECT p.name,
		COALESCE(json_extract(s.attributes, '$.title'), '') FROM sessions s
		JOIN projects p ON p.id = s.project_id WHERE s.id = ?`, sessionID).
		Scan(&project, &title)
	if err != nil {
		return "", fmt.Errorf("load chat output name: %w", err)
	}

	return chatDisplayName(project, title), nil
}

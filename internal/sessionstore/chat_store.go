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

// ChatOutput includes non-rendering lifecycle rows that still advance the chat watermark.
type ChatOutput struct {
	ID        int64
	SessionID int64
	Type      OutputType
	Content   string
}

// ChatSession separates the assistant's display identity from its durable session ID.
type ChatSession struct {
	ID             int64
	SessionID      int64
	Name           string
	Unread         int64
	LatestUnread   string
	LatestUnreadID int64
}

// ChatFocus returns zero for the assistant, including a missing chat binding.
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
	result, err := func() (sql.Result, error) {
		if sessionID == 0 {
			return s.db.ExecContext(ctx, `INSERT INTO chat_focus (manager_id, chat_id, session_id)
				VALUES (?, ?, NULL)
				ON CONFLICT(manager_id, chat_id) DO UPDATE SET session_id = NULL`, managerID, chatID)
		}

		return s.db.ExecContext(ctx, `INSERT INTO chat_focus (manager_id, chat_id, session_id)
			SELECT ?, ?, CASE WHEN s.agent_type = 'assistant' THEN NULL ELSE s.id END
			FROM sessions s WHERE s.id = ? AND `+liveChatRootSQL+`
			ON CONFLICT(manager_id, chat_id) DO UPDATE SET session_id = excluded.session_id`,
			managerID, chatID, sessionID, managerID)
	}()
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
	result, err := s.db.ExecContext(ctx, `UPDATE chat_focus SET session_id = NULL
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

// ChatSeen returns zero until this chat has consumed an output from the session.
func (s *Store) ChatSeen(ctx context.Context, managerID, chatID string, sessionID int64) (int64, error) {
	var watermark int64

	err := s.db.QueryRowContext(ctx, `SELECT last_outbox_id FROM chat_seen
		WHERE manager_id = ? AND chat_id = ? AND session_id = ?`, managerID, chatID, sessionID).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("load chat watermark: %w", err)
	}

	return watermark, nil
}

// MarkChatSeen never moves a watermark backwards, including a delayed acknowledgement.
func (s *Store) MarkChatSeen(ctx context.Context, managerID, chatID string, sessionID, outboxID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_seen (manager_id, chat_id, session_id, last_outbox_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(manager_id, chat_id, session_id) DO UPDATE
		SET last_outbox_id = MAX(chat_seen.last_outbox_id, excluded.last_outbox_id)`,
		managerID, chatID, sessionID, outboxID)
	if err != nil {
		return fmt.Errorf("advance chat watermark: %w", err)
	}

	return nil
}

// ChatOutputs includes lifecycle rows so delivery can consume their positions in the log.
func (s *Store) ChatOutputs(
	ctx context.Context,
	managerID, chatID string,
	sessionID int64,
	limit int,
) ([]ChatOutput, error) {
	if limit <= 0 {
		return nil, errors.New("chat output limit must be positive")
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, type, content FROM session_outbox
		WHERE session_id = ? AND id > COALESCE((SELECT last_outbox_id FROM chat_seen
			WHERE manager_id = ? AND chat_id = ? AND session_id = ?), 0)
		ORDER BY id LIMIT ?`, sessionID, managerID, chatID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("load unread chat outputs: %w", err)
	}
	defer rows.Close()

	var outputs []ChatOutput

	for rows.Next() {
		var output ChatOutput
		if err := rows.Scan(&output.ID, &output.SessionID, &output.Type, &output.Content); err != nil {
			return nil, fmt.Errorf("scan unread chat output: %w", err)
		}

		outputs = append(outputs, output)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unread chat outputs: %w", err)
	}

	return outputs, nil
}

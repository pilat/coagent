package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type InputSource string

const (
	InputSourceUser  InputSource = "user"
	InputSourceAgent InputSource = "agent"
)

type InputState string

const (
	InputStatePending   InputState = "pending"
	InputStateAccepted  InputState = "accepted"
	InputStateHandled   InputState = "handled"
	InputStateRejected  InputState = "rejected"
	InputStateCancelled InputState = "cancelled"
)

var (
	ErrNoPendingInput           = errors.New("session has no pending input")
	ErrInputNotFound            = errors.New("session input not found")
	ErrInputResolved            = errors.New("session input already resolved")
	ErrSessionNotAcceptingInput = errors.New("session is not accepting input")
)

const inboxColumns = `id, session_id, source, raw_content, attributes, received_at, state, resolved_at, resolution_reason, accepted_message_id`

type InboxInput struct {
	ID                int64
	SessionID         int64
	Source            InputSource
	RawContent        string
	Attributes        map[string]any
	ReceivedAt        time.Time
	State             InputState
	ResolvedAt        *time.Time
	ResolutionReason  string
	AcceptedMessageID int64
}

// InboxStore persists controller-accepted input before any runner observes it.
type InboxStore interface {
	EnqueueInput(ctx context.Context, sessionID int64, source InputSource, rawContent string) (*InboxInput, error)
	PeekPending(ctx context.Context, sessionID int64) (*InboxInput, error)
	PromoteInput(ctx context.Context, inputID int64, preparedContent string) (*StoredMessage, error)
	HandleInput(ctx context.Context, inputID int64, reason string) error
	RejectInput(ctx context.Context, inputID int64, reason string) error
	CancelPendingInputs(ctx context.Context, sessionIDs []int64, reason string) (int64, error)
	HasAcceptedInput(ctx context.Context, sessionID int64) (bool, error)
	ListSessionsWithRecoverableInput(ctx context.Context) ([]int64, error)
}

// HandleInput resolves a controller command without inserting it into the model
// transcript. Commands share the durable FIFO but stay on the control plane.
func (s *store) HandleInput(ctx context.Context, inputID int64, reason string) error {
	if reason == "" {
		return errors.New("empty input handling reason")
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'handled', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND state = 'pending'`,
		time.Now().UTC(), reason, inputID,
	)
	if err != nil {
		return fmt.Errorf("handle input %d: %w", inputID, err)
	}

	return requireOnePendingResolution(ctx, s.db, result, inputID)
}

func (s *store) EnqueueInput(
	ctx context.Context,
	sessionID int64,
	source InputSource,
	rawContent string,
) (*InboxInput, error) {
	if source != InputSourceUser && source != InputSourceAgent {
		return nil, fmt.Errorf("invalid input source %q", source)
	}

	if rawContent == "" {
		return nil, errors.New("empty input content")
	}

	receivedAt := time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO session_inbox (session_id, source, raw_content, attributes, received_at)
		SELECT id, ?, ?, CASE
			WHEN ? = 'user' AND json_type(attributes, '$.manager_id') = 'text'
				AND json_extract(attributes, '$.manager_id') <> ''
			THEN json_object('manager_id', json_extract(attributes, '$.manager_id'))
			ELSE '{}'
		END, ? FROM sessions
		WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('killed', 'terminating', 'stopping')`,
		source, rawContent, source, receivedAt, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("enqueue input for session %d: %w", sessionID, err)
	}

	inputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("input id: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("enqueue rows affected: %w", err)
	}

	if affected == 0 {
		return nil, fmt.Errorf("%w: session %d", ErrSessionNotAcceptingInput, sessionID)
	}

	attributes := map[string]any{}

	if source == InputSourceUser {
		var owner sql.NullString
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT json_extract(attributes, '$.manager_id') FROM sessions WHERE id = ?`,
			sessionID,
		).Scan(&owner); err != nil {
			return nil, fmt.Errorf("load input owner: %w", err)
		}

		if owner.Valid && owner.String != "" {
			attributes["manager_id"] = owner.String
		}
	}

	return &InboxInput{
		ID:         inputID,
		SessionID:  sessionID,
		Source:     source,
		RawContent: rawContent,
		Attributes: attributes,
		ReceivedAt: receivedAt,
		State:      InputStatePending,
	}, nil
}

// PeekPending returns the oldest pending input.
func (s *store) PeekPending(ctx context.Context, sessionID int64) (*InboxInput, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+inboxColumns+` FROM session_inbox
		 WHERE session_id = ? AND state = 'pending' ORDER BY id LIMIT 1`,
		sessionID,
	)

	input, err := scanInboxInput(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPendingInput
	}

	if err != nil {
		return nil, fmt.Errorf("peek pending input for session %d: %w", sessionID, err)
	}

	return input, nil
}

func (s *store) PromoteInput(ctx context.Context, inputID int64, preparedContent string) (*StoredMessage, error) {
	if preparedContent == "" {
		return nil, errors.New("empty prepared input content")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin promote input: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, err
	}

	if input.State == InputStateAccepted {
		return loadMessage(ctx, tx, input.AcceptedMessageID)
	}

	if input.State != InputStatePending {
		return nil, fmt.Errorf("%w: input %d is %s", ErrInputResolved, inputID, input.State)
	}

	now := time.Now().UTC()

	msg, err := insertPromotedMessage(ctx, tx, input, preparedContent)
	if err != nil {
		return nil, err
	}

	if err := acceptPendingInput(ctx, tx, inputID, msg.ID, now); err != nil {
		return nil, err
	}

	if err := activatePromotedInputSession(ctx, tx, input.SessionID, inputID, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit promote input %d: %w", inputID, err)
	}

	return msg, nil
}

func (s *store) RejectInput(ctx context.Context, inputID int64, reason string) error {
	if reason == "" {
		return errors.New("empty input rejection reason")
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'rejected', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND state = 'pending'`,
		time.Now().UTC(), reason, inputID,
	)
	if err != nil {
		return fmt.Errorf("reject input %d: %w", inputID, err)
	}

	return requireOnePendingResolution(ctx, s.db, result, inputID)
}

func (s *store) CancelPendingInputs(
	ctx context.Context,
	sessionIDs []int64,
	reason string,
) (int64, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}

	if reason == "" {
		return 0, errors.New("empty input cancellation reason")
	}

	sessionIDsJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("marshal session ids: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'cancelled', resolved_at = ?, resolution_reason = ?
		WHERE state = 'pending'
			AND session_id IN (SELECT value FROM json_each(?))`,
		time.Now().UTC(), reason, sessionIDsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("cancel pending inputs: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel inputs rows affected: %w", err)
	}

	return affected, nil
}

func (s *store) ListSessionsWithRecoverableInput(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, recoverableInputQuery)
	if err != nil {
		return nil, fmt.Errorf("list sessions with recoverable input: %w", err)
	}
	defer rows.Close()

	return scanSessionIDs(rows, "recoverable")
}

func (s *store) HasAcceptedInput(ctx context.Context, sessionID int64) (bool, error) {
	var accepted bool
	if err := s.db.QueryRowContext(ctx, acceptedInputExistsQuery, sessionID).Scan(&accepted); err != nil {
		return false, fmt.Errorf("check accepted input for session %d: %w", sessionID, err)
	}

	return accepted, nil
}

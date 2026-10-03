package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	InputSourceUser       InputSource = "user"
	InputSourceAgent      InputSource = "agent"
	InputSourceProcess    InputSource = "process"
	InputSourceSubagent   InputSource = "subagent"
	InputSourceSchedule   InputSource = "schedule"
	InputSourceCallResult InputSource = "call_result"
)

const (
	InputStatePending   InputState = "pending"
	InputStateAccepted  InputState = "accepted"
	InputStateHandled   InputState = "handled"
	InputStateRejected  InputState = "rejected"
	InputStateCancelled InputState = "cancelled"
)

const inboxColumns = `id, session_id, source, raw_content, attributes, received_at, state, resolved_at, resolution_reason, accepted_message_id, COALESCE(delivery_key, '')`

var (
	ErrNoPendingInput           = errors.New("session has no pending input")
	ErrInputNotFound            = errors.New("session input not found")
	ErrInputResolved            = errors.New("session input already resolved")
	ErrSessionNotAcceptingInput = errors.New("session is not accepting input")
)

type InputSource string

type InputState string

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
	DeliveryKey       string
}

// HasPendingAsyncInputByRoot reports process or subagent input addressed to the root tree.
//
//nolint:wsl_v5 // The EXISTS query is one bounded projection.
func (s *Store) HasPendingAsyncInputByRoot(ctx context.Context, rootID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM session_inbox input
		JOIN sessions owner ON owner.id = input.session_id
		WHERE (owner.id = ? OR owner.root_id = ?)
			AND input.state = 'pending' AND input.source IN ('process', 'subagent')
	)`, rootID, rootID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query pending async input by root: %w", err)
	}

	return exists, nil
}

func (s *Store) PeekPending(ctx context.Context, sessionID int64) (*InboxInput, error) {
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

func (s *Store) ListSessionsWithRecoverableInput(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, recoverableInputQuery)
	if err != nil {
		return nil, fmt.Errorf("list sessions with recoverable input: %w", err)
	}
	defer rows.Close()

	return scanSessionIDs(rows, "recoverable")
}

func (s *Store) HasAcceptedInput(ctx context.Context, sessionID int64) (bool, error) {
	var accepted bool
	if err := s.db.QueryRowContext(ctx, acceptedInputExistsQuery, sessionID).Scan(&accepted); err != nil {
		return false, fmt.Errorf("check accepted input for session %d: %w", sessionID, err)
	}

	return accepted, nil
}

func (s *Store) CancelPendingInputs(
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

// CancelPendingInputsForStop preserves independently committed completion
// facts. A later explicit user resume observes them in the original FIFO.
func (s *Store) CancelPendingInputsForStop(
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

	result, err := s.db.ExecContext(ctx, `UPDATE session_inbox
		SET state = 'cancelled', resolved_at = ?, resolution_reason = ?
		WHERE state = 'pending'
			AND source NOT IN ('schedule', 'process', 'subagent')
			AND session_id IN (SELECT value FROM json_each(?))`,
		time.Now().UTC(), reason, sessionIDsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("cancel stopped session inputs: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel stopped inputs rows affected: %w", err)
	}

	return affected, nil
}

// readOnlyCommandReceipts mirrors the recovery query's read-only set: these
// manager-owned inputs answer the manager directly and owe no model reply.
func readOnlyCommandReceipt(content string) bool {
	trimmed := strings.TrimSpace(content)

	switch {
	case trimmed == "/status", trimmed == "/help", trimmed == "/schedules",
		trimmed == "/compact", strings.HasPrefix(trimmed, "/compact "):
		return true
	default:
		return false
	}
}

func insertActivation(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	draft ActivationDraft,
	now time.Time,
) (*ToolActivation, error) {
	owner, _ := input.Attributes[managerIDAttribute].(string)
	if input.Source != InputSourceUser || owner == "" {
		return nil, ErrActivationConflict
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO session_tool_activations
		(input_id, session_id, tool_id, command, state, created_at)
		SELECT ?, sessions.id, ?, ?, 'pending', ? FROM sessions
		WHERE sessions.id = ? AND sessions.parent_id = 0
			AND json_extract(sessions.attributes, '$.manager_id') = ?`,
		input.ID, draft.ToolID, draft.Command, now, input.SessionID, owner)
	if err != nil {
		return nil, fmt.Errorf("insert tool activation: %w", err)
	}

	if err := requireActivationChanged(result); err != nil {
		return nil, err
	}

	return &ToolActivation{
		InputID: input.ID, SessionID: input.SessionID, ToolID: draft.ToolID,
		Command: draft.Command, State: ActivationPending, CreatedAt: now,
	}, nil
}

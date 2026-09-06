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

//nolint:wsl_v5 // Validation, update, and affected-row accounting form one cancellation operation.
func (s *store) CancelPendingInputsPreservingShieldCommands(
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

	encoded, err := json.Marshal(sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("marshal shield-preserving session ids: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_inbox
		SET state = 'cancelled', resolved_at = ?, resolution_reason = ?
		WHERE state = 'pending' AND session_id IN (SELECT value FROM json_each(?))
			AND NOT (source = 'user' AND trim(raw_content) IN ('/shieldsup', '/shieldsdown')
				AND json_type(attributes, '$.manager_id') = 'text'
				AND json_extract(attributes, '$.manager_id') <> '')`,
		time.Now().UTC(), reason, encoded)
	if err != nil {
		return 0, fmt.Errorf("cancel pending inputs while preserving shields: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("shield-preserving cancellation rows affected: %w", err)
	}

	return affected, nil
}

//nolint:wsl_v5 // Ordered row scanning is one recovery projection.
func (s *store) SelectInterruptedShieldRaises(ctx context.Context) ([]InterruptedShieldRaise, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.session_id, i.id
		FROM session_inbox i
		WHERE i.state = 'handled' AND i.resolution_reason = 'shieldsup'
			AND EXISTS (SELECT 1 FROM session_outbox o WHERE o.session_id = i.session_id
				AND o.source_key = 'input:' || i.id || ':shieldsup:started')
			AND NOT EXISTS (SELECT 1 FROM session_outbox o WHERE o.session_id = i.session_id
				AND o.source_key = 'input:' || i.id || ':shieldsup:completed')
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("list interrupted shield raises: %w", err)
	}
	defer rows.Close()

	var raises []InterruptedShieldRaise
	for rows.Next() {
		var raise InterruptedShieldRaise
		if err := rows.Scan(&raise.SessionID, &raise.InputID); err != nil {
			return nil, fmt.Errorf("scan interrupted shield raise: %w", err)
		}
		raises = append(raises, raise)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interrupted shield raises: %w", err)
	}

	return raises, nil
}

//nolint:wsl_v5 // Input, owner, and command checks form one authorization boundary.
func loadShieldInput(
	ctx context.Context,
	tx *sql.Tx,
	inputID int64,
	command string,
	reason string,
) (*InboxInput, string, bool, error) {
	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, "", false, err
	}
	if input.Source != InputSourceUser || strings.TrimSpace(input.RawContent) != command {
		return nil, "", false, ErrInvalidShieldCommand
	}

	owner, err := outputOwner(ctx, tx, input.SessionID)
	if err != nil {
		return nil, "", false, err
	}
	inputOwner, _ := input.Attributes[managerIDAttribute].(string)
	if inputOwner == "" || inputOwner != owner {
		return nil, "", false, ErrInvalidShieldCommand
	}

	switch input.State {
	case InputStatePending:
		return input, owner, false, nil
	case InputStateHandled:
		if input.ResolutionReason == reason {
			return input, owner, true, nil
		}
	case InputStateAccepted, InputStateRejected, InputStateCancelled:
	}

	return nil, "", false, fmt.Errorf("%w: input %d is %s", ErrInvalidShieldCommand, input.ID, input.State)
}

func handleShieldInput(ctx context.Context, tx *sql.Tx, inputID int64, reason string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE session_inbox
		SET state = 'handled', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND state = 'pending'`, now, reason, inputID)
	if err != nil {
		return fmt.Errorf("handle shield input: %w", err)
	}

	return requireOnePendingResolution(ctx, tx, result, inputID)
}

//nolint:wsl_v5 // Idempotency metadata and output insertion must remain adjacent.
func insertShieldOutput(
	ctx context.Context,
	tx *sql.Tx,
	rootID, inputID int64,
	owner, suffix string,
	kind OutputType,
	content string,
	releases bool,
	now time.Time,
) (*OutputCommit, error) {
	attributes, err := stampMessageOutputAttributes(ctx, tx, rootID, owner, nil)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal shield output attributes: %w", err)
	}

	key := fmt.Sprintf("input:%d:%s", inputID, suffix)
	fingerprint := outputFingerprintWithRelease(kind, content, rootID, nil, releases)
	result, err := tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		rootID, kind, content, string(encoded), key, fingerprint, now, releases)
	if err != nil {
		return nil, fmt.Errorf("insert shield output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("shield output id: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil || affected == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, rootID, key).Scan(&outputID); err != nil {
			return nil, fmt.Errorf("load stored shield output: %w", err)
		}
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

//nolint:wsl_v5 // Replay reconstructs a single durable phase from paired outputs.
func replayShieldRaise(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	owner string,
) (*ShieldRaise, *OutputCommit, error) {
	for _, suffix := range []string{"shieldsup:completed", "shieldsup:unavailable", "shieldsup:started"} {
		var outputID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM session_outbox
			WHERE session_id = ? AND source_key = 'input:' || ? || ':' || ?`,
			input.SessionID, input.ID, suffix).Scan(&outputID)
		if err == nil {
			changed := suffix == "shieldsup:started"
			var needsStop bool
			if changed {
				if err := tx.QueryRowContext(
					ctx, `SELECT status = 'stopping' FROM sessions WHERE id = ?`, input.SessionID,
				).Scan(&needsStop); err != nil {
					return nil, nil, fmt.Errorf("load replayed shield raise status: %w", err)
				}
			}

			return &ShieldRaise{
				RootID: input.SessionID, InputID: input.ID, Changed: changed, NeedsStop: needsStop,
			}, &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("load replayed shield raise: %w", err)
		}
	}

	return nil, nil, fmt.Errorf("%w: handled raise %d has no output", ErrInvalidShieldCommand, input.ID)
}

//nolint:wsl_v5 // Replay validates the exact terminal output before reuse.
func replayShieldDown(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	owner string,
) (*OutputCommit, error) {
	for _, suffix := range []string{"shieldsdown:completed", "shieldsdown:busy"} {
		var outputID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM session_outbox
			WHERE session_id = ? AND source_key = 'input:' || ? || ':' || ?`,
			input.SessionID, input.ID, suffix).Scan(&outputID)
		if err == nil {
			return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("load replayed shield lowering: %w", err)
		}
	}

	return nil, fmt.Errorf("%w: handled lowering %d has no output", ErrInvalidShieldCommand, input.ID)
}

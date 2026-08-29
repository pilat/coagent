package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// lifecycleKill mirrors the daemon's canonical /kill command name.
const lifecycleKill = "kill"

// lifecycleStop mirrors the daemon's canonical /stop command name.
const lifecycleStop = "stop"

// BeginLifecycleInput records the durable fence before the runner is cancelled.
// The command comes from the daemon's central classification, not from re-parsing
// stored content here.
func (s *store) BeginLifecycleInput(
	ctx context.Context,
	inputID int64,
	command, content string,
) (*OutputCommit, error) {
	if content == "" {
		return nil, errors.New("empty lifecycle output")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin lifecycle input: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, err
	}

	if input.State != InputStatePending {
		return nil, fmt.Errorf("%w: input %d is %s", ErrInputResolved, inputID, input.State)
	}

	owner, err := outputOwner(ctx, tx, input.SessionID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	// Stop parks a tree it intends to finish itself; kill hands the root to boot
	// reconciliation, which selects kill cleanup by the absence of a replacement.
	fence := SessionStatusStopping
	if command == lifecycleKill {
		fence = SessionStatusTerminating
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE sessions SET status = ?, updated_at = ?
		WHERE id = ? AND killed_at IS NULL
			AND status IN ('active', 'completed', 'suspended', 'error', 'stopped')`, fence, now, input.SessionID)
	if err != nil {
		return nil, fmt.Errorf("start lifecycle fence: %w", err)
	}

	if err := requireOneSessionUpdate(result, input.SessionID); err != nil {
		return nil, err
	}

	result, err = tx.ExecContext(ctx, `
		UPDATE session_inbox SET state = 'handled', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND state = 'pending'`, now, command, inputID)
	if err != nil {
		return nil, fmt.Errorf("handle lifecycle input: %w", err)
	}

	if err := requireOnePendingResolution(ctx, tx, result, inputID); err != nil {
		return nil, err
	}

	outputID, err := insertLifecycleAcknowledgement(ctx, tx, input.SessionID, inputID, command, content, owner, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit lifecycle input: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

// insertLifecycleAcknowledgement writes the command's visible start. /stop gets
// a replaceable, non-releasing start row that a later terminal completion
// transaction edits into the final result; other lifecycle commands keep the
// persistent releasing acknowledgement.
func insertLifecycleAcknowledgement(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, inputID int64,
	command, content, owner string,
	now time.Time,
) (int64, error) {
	attributes, err := stampMessageOutputAttributes(ctx, tx, sessionID, owner, nil)
	if err != nil {
		return 0, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return 0, fmt.Errorf("marshal lifecycle input attributes: %w", err)
	}

	kind := OutputMessagePersistent
	releases := true
	key := fmt.Sprintf("input:%d:%s:result", inputID, command)

	if command == lifecycleStop {
		kind = OutputMessageReplaceable
		releases = false
		key = fmt.Sprintf("input:%d:stop:started", inputID)
	}

	fingerprint := outputFingerprintWithRelease(kind, content, sessionID, nil, releases)

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox (session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, kind, content, string(encoded), key, fingerprint, now, releases)
	if err != nil {
		return 0, fmt.Errorf("insert lifecycle acknowledgement: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("lifecycle output id: %w", err)
	}

	return outputID, nil
}

// hasReplacementRow reports whether clear already transferred this root's
// manager surface to its replacement.
func hasReplacementRow(
	ctx context.Context,
	q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	sessionID int64,
	owner string,
) (bool, error) {
	var replaced int64

	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM session_outbox
		WHERE type = 'session_replaced'
			AND json_extract(attributes, '$.old_session_id') = ?
			AND json_extract(attributes, '$.manager_id') = ?
		LIMIT 1`, sessionID, owner).Scan(&replaced)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("load replacement row: %w", err)
	}

	return true, nil
}

func insertClosedOutput(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, sessionID int64, owner string, now time.Time,
) (int64, error) {
	attributes, err := json.Marshal(map[string]any{managerIDAttribute: owner, "reason": killedReason})
	if err != nil {
		return 0, fmt.Errorf("marshal closed output attributes: %w", err)
	}

	key := fmt.Sprintf("session:%d:closed", sessionID)

	fingerprint := outputFingerprintWithRelease(
		OutputSessionClosed, "", sessionID, map[string]any{"reason": killedReason}, true,
	)

	result, err := q.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, '', ?, ?, ?, ?, 1)`,
		sessionID, OutputSessionClosed, string(attributes), key, fingerprint, now)
	if err != nil {
		return 0, fmt.Errorf("insert session closed output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("session closed output id: %w", err)
	}

	return outputID, nil
}

func (s *store) ResolveReplacement(ctx context.Context, sessionID int64, managerID string) (int64, error) {
	seen := make(map[int64]struct{})
	for range 16 {
		if _, duplicate := seen[sessionID]; duplicate {
			return 0, errors.New("session replacement cycle")
		}

		seen[sessionID] = struct{}{}

		record, err := s.GetSession(ctx, sessionID)
		if err != nil {
			return 0, err
		}

		owner, _ := record.Attributes[managerIDAttribute].(string)
		if owner != managerID {
			return 0, ErrOutputOwner
		}

		if record.Status != SessionStatusTerminating && record.KilledAt == nil {
			return sessionID, nil
		}

		var replacementID int64

		err = s.db.QueryRowContext(ctx, `
			SELECT session_id FROM session_outbox
			WHERE type = 'session_replaced'
				AND json_extract(attributes, '$.old_session_id') = ?
				AND json_extract(attributes, '$.manager_id') = ?
			ORDER BY id DESC LIMIT 1`, sessionID, managerID).Scan(&replacementID)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrSessionNotAcceptingInput
		}

		if err != nil {
			return 0, fmt.Errorf("load replacement session: %w", err)
		}

		next, err := s.GetSession(ctx, replacementID)
		if err != nil {
			return 0, err
		}

		nextOwner, _ := next.Attributes[managerIDAttribute].(string)
		if next.ParentID != 0 || next.ProjectID != record.ProjectID || nextOwner != managerID {
			return 0, errors.New("invalid replacement session")
		}

		sessionID = replacementID
	}

	return 0, errors.New("session replacement chain too deep")
}

// MarkSessionKilledWithOutput omits a close row for a terminating old root;
// clear transfers that manager surface to its replacement.
func (s *store) MarkSessionKilledWithOutput(ctx context.Context, sessionID int64) (*OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin killed output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := scanSession(
		tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, sessionID),
	)
	if err != nil {
		return nil, err
	}

	if record.KilledAt != nil {
		//nolint:nilnil // an already-closed root has no further lifecycle output.
		return nil, nil
	}

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET status = 'killed', killed_at = ?, updated_at = ? WHERE id = ? AND killed_at IS NULL`,
		now, now, sessionID)
	if err != nil {
		return nil, fmt.Errorf("mark session killed: %w", err)
	}

	if err := requireOneSessionUpdate(result, sessionID); err != nil {
		return nil, err
	}

	owner, _ := record.Attributes[managerIDAttribute].(string)

	replaced := false
	if owner != "" {
		replaced, err = hasReplacementRow(ctx, tx, sessionID, owner)
		if err != nil {
			return nil, err
		}
	}

	if record.ParentID != 0 || owner == "" || replaced {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit killed session: %w", err)
		}
		//nolint:nilnil // ownerless and replacement roots have no manager output.
		return nil, nil
	}

	outputID, err := insertClosedOutput(ctx, tx, sessionID, owner, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit killed output: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

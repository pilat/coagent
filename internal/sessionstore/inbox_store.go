package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/transcript"
)

type InputSource string

const (
	InputSourceUser     InputSource = "user"
	InputSourceAgent    InputSource = "agent"
	InputSourceProcess  InputSource = "process"
	InputSourceSubagent InputSource = "subagent"
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
type InboxStore interface { //nolint:interfacebloat // One durable FIFO boundary owns its full row lifecycle.
	EnqueueInput(ctx context.Context, sessionID int64, source InputSource, rawContent string) (*InboxInput, error)
	EnqueueAsyncInput(
		ctx context.Context,
		sessionID int64,
		source InputSource,
		rawContent string,
		attributes map[string]any,
	) (*InboxInput, error)
	PeekPending(ctx context.Context, sessionID int64) (*InboxInput, error)
	ListPendingShieldCommands(ctx context.Context, sessionID int64) ([]*InboxInput, error)
	ListRootsWithPendingShieldCommands(ctx context.Context) ([]int64, error)
	PromoteInput(ctx context.Context, inputID int64, preparedContent string) (*transcript.Message, error)
	// PromoteInputWithReceipt is PromoteInput plus one persistent output row
	// committed in the same transaction. An empty receipt content inserts no
	// output; a non-root or ownerless session resolves no receipt.
	PromoteInputWithReceipt(
		ctx context.Context,
		inputID int64,
		preparedContent string,
		receipt OutputDraft,
	) (*transcript.Message, *OutputCommit, error)
	HandleInput(ctx context.Context, inputID int64, reason string) error
	RejectInput(ctx context.Context, inputID int64, reason string) error
	CancelPendingInputs(ctx context.Context, sessionIDs []int64, reason string) (int64, error)
	CancelPendingInputsForStop(ctx context.Context, sessionIDs []int64, reason string) (int64, error)
	HasAcceptedInput(ctx context.Context, sessionID int64) (bool, error)
	ListSessionsWithRecoverableInput(ctx context.Context) ([]int64, error)
}

//nolint:wsl_v5 // Ordered scanning is one recovery projection.
func (s *store) ListRootsWithPendingShieldCommands(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input.session_id
		FROM session_inbox input
		JOIN sessions root ON root.id = input.session_id
		WHERE input.source = 'user' AND input.state = 'pending'
			AND trim(input.raw_content) IN ('/shieldsup', '/shieldsdown')
			AND root.parent_id = 0 AND root.killed_at IS NULL
			AND root.status NOT IN ('terminating', 'killed')
			AND json_type(input.attributes, '$.manager_id') = 'text'
			AND json_extract(input.attributes, '$.manager_id') =
				json_extract(root.attributes, '$.manager_id')
		GROUP BY input.session_id
		ORDER BY MIN(input.id), input.session_id`)
	if err != nil {
		return nil, fmt.Errorf("list roots with pending shield commands: %w", err)
	}
	defer rows.Close()

	var sessionIDs []int64
	for rows.Next() {
		var sessionID int64
		if err := rows.Scan(&sessionID); err != nil {
			return nil, fmt.Errorf("scan root with pending shield command: %w", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roots with pending shield commands: %w", err)
	}

	return sessionIDs, nil
}

// ListPendingShieldCommands returns manager-owned shield commands in durable order.
//
//nolint:wsl_v5 // Ordered row scanning is one durable command projection.
func (s *store) ListPendingShieldCommands(ctx context.Context, sessionID int64) ([]*InboxInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+inboxColumns+` FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'pending'
			AND trim(raw_content) IN ('/shieldsup', '/shieldsdown')
			AND json_type(attributes, '$.manager_id') = 'text'
			AND json_extract(attributes, '$.manager_id') <> ''
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list pending shield commands: %w", err)
	}
	defer rows.Close()

	var inputs []*InboxInput
	for rows.Next() {
		input, err := scanInboxInput(rows)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending shield commands: %w", err)
	}

	return inputs, nil
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

// EnqueueAsyncInput persists a completion producer fact without manager
// ownership semantics. Stopped and errored sessions retain it for explicit resume.
func (s *store) EnqueueAsyncInput(
	ctx context.Context,
	sessionID int64,
	source InputSource,
	rawContent string,
	attributes map[string]any,
) (*InboxInput, error) {
	if source != InputSourceProcess && source != InputSourceSubagent {
		return nil, fmt.Errorf("invalid asynchronous input source %q", source)
	}

	if rawContent == "" {
		return nil, errors.New("empty input content")
	}

	if attributes == nil {
		attributes = map[string]any{}
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("encode asynchronous input attributes: %w", err)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, attributes, received_at)
		SELECT id, ?, ?, ?, ? FROM sessions WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('killed', 'terminating', 'stopping')`, source, rawContent, string(encoded), now, sessionID)
	if err != nil {
		return nil, fmt.Errorf("enqueue asynchronous input for session %d: %w", sessionID, err)
	}

	if rows, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("enqueue asynchronous input rows affected: %w", err)
	} else if rows != 1 {
		return nil, fmt.Errorf("%w: session %d", ErrSessionNotAcceptingInput, sessionID)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("asynchronous input id: %w", err)
	}

	return &InboxInput{
		ID: id, SessionID: sessionID, Source: source, RawContent: rawContent,
		Attributes: attributes, ReceivedAt: now, State: InputStatePending,
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

func (s *store) PromoteInput(ctx context.Context, inputID int64, preparedContent string) (*transcript.Message, error) {
	message, _, _, err := s.promoteInput(ctx, inputID, preparedContent, nil, "")

	return message, err
}

func (s *store) PromoteInputWithReceipt(
	ctx context.Context,
	inputID int64,
	preparedContent string,
	receipt OutputDraft,
) (*transcript.Message, *OutputCommit, error) {
	if receipt.Type != OutputMessagePersistent {
		return nil, nil, fmt.Errorf("invalid promotion receipt type %q", receipt.Type)
	}

	message, _, commit, err := s.promoteInput(ctx, inputID, preparedContent, nil, receipt.Content)

	return message, commit, err
}

func (s *store) PromoteInputWithActivation(
	ctx context.Context,
	inputID int64,
	preparedContent string,
	activation ActivationDraft,
) (*transcript.Message, *ToolActivation, error) {
	if activation.ToolID == "" || activation.Command == "" || activation.Command[0] != '/' {
		return nil, nil, ErrActivationConflict
	}

	message, grant, _, err := s.promoteInput(ctx, inputID, preparedContent, &activation, "")

	return message, grant, err
}

// promoteInput accepts one pending input in one transaction: the transcript
// row, optional activation grant, input acceptance, session activation, the
// model-input generation advance, and the optional persistent receipt.
//
//nolint:funlen // The accepted-replay and fresh-promotion branches share one ordered transaction.
func (s *store) promoteInput(
	ctx context.Context,
	inputID int64,
	preparedContent string,
	activation *ActivationDraft,
	receiptContent string,
) (*transcript.Message, *ToolActivation, *OutputCommit, error) {
	if preparedContent == "" {
		return nil, nil, nil, errors.New("empty prepared input content")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin promote input: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, nil, nil, err
	}

	if input.State == InputStateAccepted {
		message, loadErr := loadMessage(ctx, tx, input.AcceptedMessageID)
		if loadErr != nil {
			return nil, nil, nil, loadErr
		}

		if activation == nil {
			return message, nil, nil, nil
		}

		existing, loadErr := scanActivation(tx.QueryRowContext(ctx, `SELECT input_id, session_id, tool_id,
			command, state, COALESCE(tool_call_id, ''), created_at, resolved_at
			FROM session_tool_activations WHERE input_id = ?`, inputID))
		if loadErr != nil {
			return nil, nil, nil, fmt.Errorf("load promoted activation: %w", loadErr)
		}

		if existing.ToolID != activation.ToolID || existing.Command != activation.Command {
			return nil, nil, nil, ErrActivationConflict
		}

		return message, existing, nil, nil
	}

	if input.State != InputStatePending {
		return nil, nil, nil, fmt.Errorf("%w: input %d is %s", ErrInputResolved, inputID, input.State)
	}

	now := time.Now().UTC()

	msg, err := insertPromotedMessage(ctx, tx, input, preparedContent)
	if err != nil {
		return nil, nil, nil, err
	}

	var grant *ToolActivation
	if activation != nil {
		grant, err = insertActivation(ctx, tx, input, *activation, now)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	if err := acceptPendingInput(ctx, tx, inputID, msg.ID, now); err != nil {
		return nil, nil, nil, err
	}

	if err := activatePromotedInputSession(ctx, tx, input.SessionID, inputID, now); err != nil {
		return nil, nil, nil, err
	}

	if err := advanceModelInputGeneration(ctx, tx, input.SessionID, msg.ID); err != nil {
		return nil, nil, nil, err
	}

	commit, err := insertPromotionReceipt(ctx, tx, input, receiptContent)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit promote input %d: %w", inputID, err)
	}

	return msg, grant, commit, nil
}

// insertPromotionReceipt inserts the promotion's persistent receipt after the
// model-input generation advanced, so the row snapshots the new generation. It
// releases the accepted command turn and is keyed by the accepted input id, so
// a replay resolves the original row instead of inserting a second receipt.
//
//nolint:nilnil // Absence of a receipt is the normal non-manager outcome, not an error.
func insertPromotionReceipt(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	content string,
) (*OutputCommit, error) {
	if content == "" {
		return nil, nil
	}

	// The owner is read live like every other message-output producer: a stale
	// manager id on the inbox row snapshot must not misattribute the receipt.
	owner, err := outputOwner(ctx, tx, input.SessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		return nil, nil //nolint:nilnil // Ownerless and subagent sessions resolve no receipt.
	}

	if err != nil {
		return nil, err
	}

	if err := outputSessionWritable(ctx, tx, input.SessionID); err != nil {
		return nil, err
	}

	attributes, err := stampMessageOutputAttributes(ctx, tx, input.SessionID, owner, nil)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal promotion receipt attributes: %w", err)
	}

	sourceKey := fmt.Sprintf("input:%d:skill_receipt", input.ID)
	fingerprint := outputFingerprintWithRelease(
		OutputMessagePersistent, content, input.SessionID, nil, true,
	)

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?, 1)`,
		input.SessionID, content, string(encoded), sourceKey, fingerprint, time.Now().UTC(),
	)
	if err == nil {
		outputID, idErr := result.LastInsertId()
		if idErr != nil {
			return nil, fmt.Errorf("promotion receipt id: %w", idErr)
		}

		return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
	}

	if !isUniqueConstraintError(err) {
		return nil, fmt.Errorf("insert promotion receipt: %w", err)
	}

	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM session_outbox
		WHERE session_id = ? AND source_key = ?`, input.SessionID, sourceKey,
	).Scan(&existing); err != nil {
		return nil, fmt.Errorf("load promotion receipt replay: %w", err)
	}

	if existing != fingerprint {
		return nil, fmt.Errorf("%w: promotion receipt for input %d", ErrOutputConflict, input.ID)
	}

	return &OutputCommit{OwnerID: owner, Existing: true}, nil
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

// CancelPendingInputsForStop preserves independently committed completion
// facts. A later explicit user resume observes them in the original FIFO.
func (s *store) CancelPendingInputsForStop(
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
			AND source NOT IN ('process', 'subagent')
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

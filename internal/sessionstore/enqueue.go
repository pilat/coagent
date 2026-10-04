package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/budget"
)

var enqueueTransactions sync.Map

type Input struct {
	SessionID   int64
	Source      InputSource
	Content     string
	Attributes  map[string]any
	DeliveryKey string
}

type Enqueued struct {
	Input   *InboxInput
	Applied bool
}

type enqueueTransaction struct {
	mu       sync.Mutex
	sessions map[int64]struct{}
}

// Enqueue commits input before waking its session; delivery keys are unique per session.
func (s *Store) Enqueue(ctx context.Context, in Input) (*Enqueued, error) {
	var result *Enqueued
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = EnqueueTx(ctx, tx, in)

		return err
	})

	return result, err
}

// EnqueueTx requires WithTx so producer facts and input become visible before waking.
func EnqueueTx(ctx context.Context, tx *sql.Tx, in Input) (*Enqueued, error) {
	registered, ok := enqueueTransactions.Load(tx)
	if !ok {
		return nil, errors.New("enqueue transaction requires WithTx")
	}

	pending, valid := registered.(*enqueueTransaction)
	if !valid {
		return nil, errors.New("invalid enqueue transaction registration")
	}

	if err := validateEnqueueInput(in); err != nil {
		return nil, err
	}
	var parentID int64
	var status SessionStatus
	var killed sql.NullTime
	var attrs string

	err := tx.QueryRowContext(ctx, `SELECT parent_id,status,killed_at,attributes FROM sessions WHERE id = ?`, in.SessionID).
		Scan(&parentID, &status, &killed, &attrs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotAcceptingInput
	}

	if err != nil {
		return nil, fmt.Errorf("load enqueue session: %w", err)
	}

	accepting, err := enqueueLifecycleAccepts(in.Source, status, killed.Valid)
	if err != nil {
		return nil, err
	}

	if !accepting {
		return &Enqueued{}, nil
	}

	duplicate, err := enqueueDuplicateTx(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	if duplicate {
		return &Enqueued{}, nil
	}

	now := time.Now().UTC()

	attributes, err := prepareEnqueueAttributesTx(ctx, tx, in, parentID, attrs, now)
	if err != nil {
		return nil, err
	}

	if in.Source == InputSourceSchedule {
		if parentID != 0 {
			return &Enqueued{}, nil
		}

		if err := activateScheduledInputTx(ctx, tx, in.SessionID, now); err != nil {
			return nil, err
		}
	}

	return insertEnqueuedInputTx(ctx, tx, in, attributes, now, pending)
}

// WithTx publishes EnqueueTx wake hints only after its transaction commits.
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pending := &enqueueTransaction{sessions: make(map[int64]struct{})}

	enqueueTransactions.Store(tx, pending)
	defer enqueueTransactions.Delete(tx)

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store transaction: %w", err)
	}

	pending.mu.Lock()
	defer pending.mu.Unlock()

	for id := range pending.sessions {
		s.recordWoken(id)
	}

	return nil
}

// Woken coalesces commit hints; TakeWoken supplies the corresponding session IDs.
func (s *Store) Woken() <-chan struct{} { return s.woken }

// TakeWoken atomically drains the committed session IDs represented by wake hints.
func (s *Store) TakeWoken() []int64 {
	s.wokenMu.Lock()
	defer s.wokenMu.Unlock()

	ids := make([]int64, 0, len(s.wokenSessions))
	for id := range s.wokenSessions {
		ids = append(ids, id)
	}

	clear(s.wokenSessions)

	return ids
}

func (s *Store) ListPending(ctx context.Context, sessionID int64) ([]*InboxInput, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+inboxColumns+` FROM session_inbox WHERE session_id = ? AND state = 'pending' ORDER BY id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending inputs: %w", err)
	}
	defer rows.Close()
	var inputs []*InboxInput

	for rows.Next() {
		in, err := scanInboxInput(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending input: %w", err)
		}

		inputs = append(inputs, in)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending session inputs: %w", err)
	}

	return inputs, nil
}

func (s *Store) CallPending(ctx context.Context, sessionID int64, callID string) bool {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages call,json_each(call.tool_calls) item
  WHERE call.session_id = ? AND call.rejected_reason IS NULL AND json_extract(item.value,'$.ID') = ?
  AND NOT EXISTS(SELECT 1 FROM messages result WHERE result.session_id = call.session_id AND result.role = 'tool' AND result.tool_call_id = ?))`, sessionID, callID, callID).Scan(&pending)

	return err == nil && pending
}

func (s *Store) recordWoken(id int64) {
	s.wokenMu.Lock()
	s.wokenSessions[id] = struct{}{}
	s.wokenMu.Unlock()

	select {
	case s.woken <- struct{}{}:
	default:
	}
}

func isExactControlCommand(content string) bool {
	if readOnlyCommandReceipt(content) {
		return true
	}

	switch strings.TrimSpace(content) {
	case "/stop", "/clear", "/kill":
		return true
	default:
		return false
	}
}

func prepareManagerModelInputTx(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET episode_started_at = ? WHERE id = ?
  AND (episode_started_at IS NULL OR status NOT IN ('active','suspended'))`, now, id); err != nil {
		return fmt.Errorf("start autonomous episode: %w", err)
	}
	var state, phase string

	err := tx.QueryRowContext(ctx, `SELECT state,park_phase FROM session_budgets WHERE root_session_id = ?`, id).
		Scan(&state, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load input budget: %w", err)
	}

	if state != "fired" {
		return nil
	}

	if phase == "draining" {
		return budget.ErrConflict
	}

	result, err := tx.ExecContext(
		ctx,
		`UPDATE session_budgets SET state = 'released',released_at = ?,released_reason = 'resumed',park_owner = ''
  WHERE root_session_id = ? AND state = 'fired' AND park_phase IN ('requested','parked')`,
		now,
		id,
	)
	if err != nil {
		return fmt.Errorf("release budget for input: %w", err)
	}

	if err := requireActivationChanged(result); err != nil {
		return budget.ErrConflict
	}

	return nil
}

func insertEnqueuedInputTx(
	ctx context.Context,
	tx *sql.Tx,
	in Input,
	attributes map[string]any,
	now time.Time,
	pending *enqueueTransaction,
) (*Enqueued, error) {
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("encode input attributes: %w", err)
	}

	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO session_inbox (session_id,source,raw_content,attributes,received_at,delivery_key)
  VALUES (?,?,?,?,?,NULLIF(?,''))`,
		in.SessionID,
		in.Source,
		in.Content,
		string(encoded),
		now,
		in.DeliveryKey,
	)
	if err != nil {
		return nil, fmt.Errorf("enqueue session input: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("input id: %w", err)
	}

	pending.mu.Lock()
	pending.sessions[in.SessionID] = struct{}{}
	pending.mu.Unlock()

	return &Enqueued{Applied: true, Input: &InboxInput{
		ID:          id,
		SessionID:   in.SessionID,
		Source:      in.Source,
		RawContent:  in.Content,
		Attributes:  attributes,
		ReceivedAt:  now,
		State:       InputStatePending,
		DeliveryKey: in.DeliveryKey,
	}}, nil
}

func validateEnqueueInput(in Input) error {
	if in.Content == "" {
		return errors.New("empty input content")
	}

	switch in.Source {
	case InputSourceUser,
		InputSourceAgent,
		InputSourceProcess,
		InputSourceSubagent,
		InputSourceSchedule,
		InputSourceCallResult:
	default:
		return fmt.Errorf("invalid input source %q", in.Source)
	}

	if in.Source == InputSourceCallResult {
		callID, _ := in.Attributes["call_id"].(string)

		toolID, _ := in.Attributes["tool_id"].(string)
		if callID == "" || toolID == "" {
			return errors.New("call result requires call_id and tool_id")
		}
	}

	return nil
}

func enqueueDuplicateTx(ctx context.Context, tx *sql.Tx, in Input) (bool, error) {
	if in.DeliveryKey == "" {
		return false, nil
	}
	var exists bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_inbox WHERE session_id = ? AND delivery_key = ?)
   OR (? = 'schedule' AND EXISTS(SELECT 1 FROM session_deliveries WHERE session_id = ? AND delivery_id = ?))`,
		in.SessionID, in.DeliveryKey, in.Source, in.SessionID, in.DeliveryKey).
		Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check input delivery: %w", err)
	}

	if exists {
		return true, nil
	}

	return false, nil
}

func prepareEnqueueAttributesTx(
	ctx context.Context,
	tx *sql.Tx,
	in Input,
	parentID int64,
	raw string,
	at time.Time,
) (map[string]any, error) {
	attributes := cloneAttributes(in.Attributes)

	owner, _ := unmarshalAttributes(raw)[managerIDAttribute].(string)
	if in.Source != InputSourceUser || owner == "" {
		return attributes, nil
	}

	attributes[managerIDAttribute] = owner

	if parentID == 0 && !isExactControlCommand(in.Content) {
		if err := prepareManagerModelInputTx(ctx, tx, in.SessionID, at); err != nil {
			return nil, err
		}
	}

	return attributes, nil
}

func activateScheduledInputTx(ctx context.Context, tx *sql.Tx, id int64, at time.Time) error {
	if err := startScheduledEpisode(ctx, tx, id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'active',updated_at = ? WHERE id = ? AND status = 'stopped'`,
		at,
		id,
	); err != nil {
		return fmt.Errorf("activate scheduled root: %w", err)
	}

	return nil
}

func enqueueLifecycleAccepts(source InputSource, status SessionStatus, killed bool) (bool, error) {
	producer := source == InputSourceProcess || source == InputSourceSubagent

	blocked := killed || status == SessionStatusKilled || status == SessionStatusTerminating ||
		(status == SessionStatusStopping && !producer)
	if !blocked {
		return true, nil
	}

	if producer {
		return false, nil
	}

	return false, ErrSessionNotAcceptingInput
}

package subagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/transcript"
)

const defaultReasoningLevel = "medium"

var _ Transactions = (*transactions)(nil)

type transactions struct {
	db *sql.DB
}

// NewTransactions creates the atomic subagent transition boundary.
func NewTransactions(db *sql.DB) Transactions {
	return &transactions{db: db}
}

func (s *transactions) Create(ctx context.Context, create Create) (int64, error) {
	if create.ReasoningLevel == "" {
		create.ReasoningLevel = defaultReasoningLevel
	}

	if create.State == "" {
		return 0, errors.New("create subagent: empty initial link state")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin create subagent tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	childID, err := insertSession(ctx, tx, create, now)
	if err != nil {
		return 0, err
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, timeout_sec, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		create.ParentID, childID, create.TaskCallID, create.Blocking,
		create.Depth, create.State, create.TimeoutSec, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("insert subagent link: %w", err)
	}

	if create.InitialInput != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_inbox
			(session_id, source, raw_content, received_at)
			VALUES (?, 'agent', ?, ?)`, childID, create.InitialInput, now); err != nil {
			return 0, fmt.Errorf("insert subagent initial input: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit create subagent: %w", err)
	}

	return childID, nil
}

func insertSession(ctx context.Context, tx *sql.Tx, create Create, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ? AND status NOT IN ('stopping', 'stopped'))`,
		create.ProjectID, create.ParentID, create.RootID, create.AgentType, create.Model,
		create.ReasoningLevel, now, now, create.ParentID)
	if err != nil {
		return 0, fmt.Errorf("insert subagent session: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check parent session admission: %w", err)
	}

	if rows == 0 {
		return 0, fmt.Errorf("parent session %d is not accepting subagents", create.ParentID)
	}

	childID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("subagent session id: %w", err)
	}

	return childID, nil
}

func (s *transactions) TryFinalizeActivation(
	ctx context.Context,
	childID int64,
	state State,
	result string,
	outcome Outcome,
) (bool, error) {
	if state != StateCompleted && state != StateError {
		return false, fmt.Errorf("invalid activation terminal state %q", state)
	}

	if outcome != OutcomeCompleted && outcome != OutcomeError && outcome != OutcomeIncomplete {
		return false, fmt.Errorf("invalid activation outcome %q", outcome)
	}

	execResult, err := s.db.ExecContext(ctx, `UPDATE subagent_links
		SET state = ?, result = ?, outcome = ?
		WHERE child_id = ? AND state IN ('spawned', 'running')
			AND EXISTS (SELECT 1 FROM sessions sess
				WHERE sess.id = subagent_links.child_id
					AND sess.status NOT IN ('stopping', 'stopped', 'killed')
					AND sess.killed_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending')`,
		state, result, outcome, childID)
	if err != nil {
		return false, fmt.Errorf("conditionally finalize subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finalize subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}

func (s *transactions) RearmDeliveredWithPendingInput(ctx context.Context, childID int64) (bool, error) {
	execResult, err := s.db.ExecContext(ctx, `UPDATE subagent_links
		SET state = 'running', blocking = 0, activation_seq = activation_seq + 1,
			delivered_at = NULL, delivered_msg_id = NULL
		WHERE child_id = ? AND state IN ('completed', 'error') AND delivered_at IS NOT NULL
			AND EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending')`, childID)
	if err != nil {
		return false, fmt.Errorf("rearm delivered subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rearm subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}

// DeliverCompletion commits the exact activation CAS and parent messages together.
func (s *transactions) DeliverCompletion(
	ctx context.Context,
	parentID int64,
	messages []*transcript.Message,
	childID int64,
	activationSeq int64,
) ([]int64, bool, error) {
	if len(messages) == 0 {
		return nil, false, fmt.Errorf("deliver completion for child %d: no messages", childID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin completion tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE subagent_links SET delivered_at = ?
		WHERE child_id = ? AND parent_id = ? AND activation_seq = ? AND delivered_at IS NULL`,
		time.Now().UTC().Unix(), childID, parentID, activationSeq)
	if err != nil {
		return nil, false, fmt.Errorf("cas delivered_at: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("completion rows affected: %w", err)
	}

	if affected == 0 {
		if err := validateCompletionParent(ctx, tx, childID, parentID); err != nil {
			return nil, false, err
		}

		return nil, false, nil
	}

	messageIDs, err := insertCompletionMessages(ctx, tx, parentID, childID, messages)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit completion: %w", err)
	}

	return messageIDs, true, nil
}

func validateCompletionParent(ctx context.Context, tx *sql.Tx, childID, parentID int64) error {
	var actual int64

	err := tx.QueryRowContext(ctx, `SELECT parent_id FROM subagent_links WHERE child_id = ?`, childID).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("subagent link for child %d not found", childID)
	}

	if err != nil {
		return fmt.Errorf("load completion link: %w", err)
	}

	if actual != parentID {
		return fmt.Errorf("child %d belongs to parent %d, not session %d", childID, actual, parentID)
	}

	return nil
}

func insertCompletionMessages(
	ctx context.Context,
	tx *sql.Tx,
	parentID, childID int64,
	messages []*transcript.Message,
) ([]int64, error) {
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		id, err := insertMessage(ctx, tx, parentID, message)
		if err != nil {
			return nil, fmt.Errorf("insert completion message: %w", err)
		}

		ids = append(ids, id)
	}

	_, err := tx.ExecContext(ctx, `UPDATE subagent_links SET delivered_msg_id = ? WHERE child_id = ?`,
		ids[len(ids)-1], childID)
	if err != nil {
		return nil, fmt.Errorf("set delivered_msg_id: %w", err)
	}

	return ids, nil
}

func insertMessage(
	ctx context.Context,
	tx *sql.Tx,
	parentID int64,
	message *transcript.Message,
) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_name, tool_calls, reasoning_content,
		 reasoning_raw, attachments, cost_usd, usage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, parentID, message.Role, message.Content,
		nullString(message.ToolCallID), nullString(message.ToolName), nullRaw(message.ToolCalls),
		message.ReasoningContent, nullRaw(message.ReasoningRaw), nullRaw(message.Attachments),
		message.CostUSD, nullRaw(message.Usage))
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("completion message id: %w", err)
	}

	return id, nil
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullRaw(value json.RawMessage) sql.NullString {
	return sql.NullString{String: string(value), Valid: len(value) > 0 && string(value) != "null"}
}

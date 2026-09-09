package subagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
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
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		create.ParentID, childID, create.TaskCallID, create.Blocking,
		create.Depth, create.State, now.Unix())
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
		(project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at, shields_up)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, parent.shields_up
		FROM sessions parent
		WHERE parent.id = ? AND parent.status NOT IN ('stopping', 'stopped')`,
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
			delivered_at = NULL, delivered_msg_id = NULL, delivered_input_id = NULL
		WHERE child_id = ? AND delivered_at IS NOT NULL
			AND ((state = 'completed' AND EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending'))
			OR (state = 'error' AND EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending'
					AND input.source IN ('user', 'agent'))))`, childID)
	if err != nil {
		return false, fmt.Errorf("rearm delivered subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rearm subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}

// DeliverBackgroundCompletion atomically transfers a terminal child outcome
// into its parent's FIFO inbox. The inbox row is the durable delivery ACK.
//
//nolint:funlen,gocyclo // Validation, suppression, insert, and ACK are one SQLite transaction.
func (s *transactions) DeliverBackgroundCompletion(ctx context.Context, link Link, iterations int) (bool, error) {
	if link.Blocking {
		return false, fmt.Errorf("background delivery for blocking child %d", link.ChildID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin background completion tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var deliveredAt sql.NullInt64
	var blocking bool
	var state string
	var activationSeq int64

	err = tx.QueryRowContext(ctx, `SELECT delivered_at, blocking, state, activation_seq
		FROM subagent_links WHERE child_id = ? AND parent_id = ?`, link.ChildID, link.ParentID).
		Scan(&deliveredAt, &blocking, &state, &activationSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("background completion link %d not found for parent %d", link.ChildID, link.ParentID)
	}

	if err != nil {
		return false, fmt.Errorf("load background completion link: %w", err)
	}

	if deliveredAt.Valid {
		return false, nil
	}

	if blocking || activationSeq != link.ActivationSeq ||
		(State(state) != StateCompleted && State(state) != StateError) {
		return false, fmt.Errorf("background completion link %d changed before delivery", link.ChildID)
	}

	var parentKilled bool

	err = tx.QueryRowContext(ctx, `SELECT
			parent.killed_at IS NOT NULL OR parent.status IN ('terminating', 'killed')
			OR root.killed_at IS NOT NULL OR root.status IN ('terminating', 'killed')
		FROM sessions parent
		JOIN sessions root ON root.id = CASE
			WHEN parent.parent_id = 0 THEN parent.id ELSE parent.root_id
		END
		WHERE parent.id = ?`, link.ParentID).
		Scan(&parentKilled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("background completion parent %d not found", link.ParentID)
	}

	if err != nil {
		return false, fmt.Errorf("load background completion parent: %w", err)
	}

	now := time.Now().UTC()
	if parentKilled {
		res, err := tx.ExecContext(ctx, `UPDATE subagent_links SET delivered_at = ?
			WHERE child_id = ? AND parent_id = ? AND activation_seq = ? AND delivered_at IS NULL`,
			now.Unix(), link.ChildID, link.ParentID, link.ActivationSeq)
		if err != nil {
			return false, fmt.Errorf("suppress killed-parent completion: %w", err)
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("suppressed completion rows affected: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit suppressed completion: %w", err)
		}

		return rows == 1, nil
	}

	attrs, err := json.Marshal(map[string]any{"child_id": link.ChildID, "activation_seq": link.ActivationSeq})
	if err != nil {
		return false, fmt.Errorf("encode subagent completion attributes: %w", err)
	}

	result := link.Result
	if result == "" {
		result = "(no output)"
	}

	content := strings.Join([]string{
		"<subagent_completion>",
		"child_id: " + strconv.FormatInt(link.ChildID, 10),
		"activation_seq: " + strconv.FormatInt(
			link.ActivationSeq,
			10,
		),
		"outcome: " + html.EscapeString(string(link.Outcome)),
		"iterations: " + strconv.Itoa(iterations),
		"result:",
		html.EscapeString(result),
		"</subagent_completion>",
	}, "\n")

	insert, err := tx.ExecContext(
		ctx,
		`INSERT INTO session_inbox (session_id, source, raw_content, attributes, received_at)
		VALUES (?, 'subagent', ?, ?, ?)`,
		link.ParentID,
		content,
		string(attrs),
		now,
	)
	if err != nil {
		return false, fmt.Errorf("insert background completion input: %w", err)
	}

	inputID, err := insert.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("background completion input id: %w", err)
	}

	res, err := tx.ExecContext(ctx, `UPDATE subagent_links
		SET delivered_at = ?, delivered_input_id = ?
		WHERE child_id = ? AND parent_id = ? AND activation_seq = ? AND delivered_at IS NULL
			AND EXISTS (SELECT 1 FROM session_inbox WHERE id = ? AND session_id = ? AND source = 'subagent'
				AND json_extract(attributes, '$.child_id') = ? AND json_extract(attributes, '$.activation_seq') = ?)`,
		now.Unix(), inputID, link.ChildID, link.ParentID, link.ActivationSeq,
		inputID, link.ParentID, link.ChildID, link.ActivationSeq)
	if err != nil {
		return false, fmt.Errorf("ack background completion input: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("background completion rows affected: %w", err)
	}

	if rows != 1 {
		return false, fmt.Errorf("background completion link %d changed during delivery", link.ChildID)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit background completion: %w", err)
	}

	return true, nil
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

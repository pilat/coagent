package subagent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const defaultReasoningLevel = "medium"

var _ Transactions = (*transactions)(nil)

type transactions struct {
	db       *sql.DB
	sessions sessionstore.Store
}

func NewTransactions(db *sql.DB, sessions sessionstore.Store) Transactions {
	return &transactions{db: db, sessions: sessions}
}

func (s *transactions) Create(ctx context.Context, create Create) (int64, error) {
	if create.ReasoningLevel == "" {
		create.ReasoningLevel = defaultReasoningLevel
	}
	if create.State == "" {
		return 0, errors.New("create subagent: empty initial link state")
	}
	var childID int64
	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		childID, err = sessionstore.CreateSubagentSessionTx(ctx, tx, sessionstore.CreateSubagentSession{
			ProjectID: create.ProjectID, ParentID: create.ParentID, RootID: create.RootID,
			AgentType: create.AgentType, Model: create.Model, ReasoningLevel: create.ReasoningLevel,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO subagent_links
   (parent_id,child_id,task_call_id,blocking,depth,state,created_at) VALUES (?,?,?,?,?,?,?)`,
			create.ParentID, childID, create.TaskCallID, create.Blocking, create.Depth, create.State, time.Now().UTC().Unix())
		if err != nil {
			return fmt.Errorf("insert subagent link: %w", err)
		}
		if create.InitialInput != "" {
			_, err = sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{SessionID: childID, Source: sessionstore.InputSourceAgent, Content: create.InitialInput})
		}
		return err
	})
	return childID, err
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

func (s *transactions) DeliverBackgroundCompletion(ctx context.Context, link Link, iterations int) (bool, error) {
	if link.Blocking {
		return false, fmt.Errorf("background delivery for blocking child %d", link.ChildID)
	}
	content := formatBackgroundCompletion(link, iterations)
	return s.deliver(ctx, link, sessionstore.Input{
		SessionID: link.ParentID, Source: sessionstore.InputSourceSubagent, Content: content,
		Attributes:  map[string]any{"child_id": link.ChildID, "activation_seq": link.ActivationSeq},
		DeliveryKey: fmt.Sprintf("subagent:%d:%d", link.ChildID, link.ActivationSeq),
	})
}

func (s *transactions) DeliverCompletion(ctx context.Context, link Link, content string) (bool, error) {
	if !link.Blocking {
		return false, fmt.Errorf("blocking delivery for background child %d", link.ChildID)
	}
	return s.deliver(ctx, link, sessionstore.Input{
		SessionID: link.ParentID, Source: sessionstore.InputSourceCallResult, Content: content,
		Attributes:  map[string]any{"call_id": link.TaskCallID, "tool_id": tool.IDTask},
		DeliveryKey: fmt.Sprintf("task:%d:%d", link.ChildID, link.ActivationSeq),
	})
}

func (s *transactions) deliver(ctx context.Context, link Link, input sessionstore.Input) (bool, error) {
	var won bool
	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var parentID, seq int64
		var delivered sql.NullInt64
		var blocking bool
		var state string
		err := tx.QueryRowContext(ctx, `SELECT parent_id,activation_seq,delivered_at,blocking,state
   FROM subagent_links WHERE child_id = ?`, link.ChildID).Scan(&parentID, &seq, &delivered, &blocking, &state)
		if err != nil {
			return fmt.Errorf("load completion link: %w", err)
		}
		if parentID != link.ParentID {
			return fmt.Errorf("child %d belongs to parent %d, not session %d", link.ChildID, parentID, link.ParentID)
		}
		if delivered.Valid || seq != link.ActivationSeq {
			return nil
		}
		if blocking != link.Blocking || !validTerminalLink(State(state), link.Outcome) {
			return fmt.Errorf("completion link %d changed before delivery", link.ChildID)
		}
		var killed bool
		err = tx.QueryRowContext(ctx, `SELECT
			parent.killed_at IS NOT NULL OR parent.status IN ('terminating', 'killed')
			OR root.killed_at IS NOT NULL OR root.status IN ('terminating', 'killed')
			FROM sessions parent
			JOIN sessions root ON root.id = CASE
				WHEN parent.parent_id = 0 THEN parent.id ELSE parent.root_id END
			WHERE parent.id = ?`, link.ParentID).Scan(&killed)
		if err != nil {
			return fmt.Errorf("load completion parent lifecycle: %w", err)
		}
		queued := &sessionstore.Enqueued{}
		if !killed {
			queued, err = sessionstore.EnqueueTx(ctx, tx, input)
			if err != nil {
				return err
			}
		}
		var inputID any
		if queued.Input != nil {
			inputID = queued.Input.ID
		}
		result, err := tx.ExecContext(ctx, `UPDATE subagent_links SET delivered_at = ?, delivered_input_id = ?
   WHERE child_id = ? AND parent_id = ? AND activation_seq = ? AND delivered_at IS NULL`,
			time.Now().UTC().Unix(), inputID, link.ChildID, link.ParentID, link.ActivationSeq)
		if err != nil {
			return fmt.Errorf("ack completion input: %w", err)
		}
		n, err := result.RowsAffected()
		won = n == 1 && queued.Applied
		return err
	})
	return won, err
}

func formatBackgroundCompletion(link Link, iterations int) string {
	result := link.Result
	if result == "" {
		result = "(no output)"
	}
	return strings.Join([]string{
		"<subagent_completion>", "child_id: " + strconv.FormatInt(link.ChildID, 10),
		"activation_seq: " + strconv.FormatInt(link.ActivationSeq, 10),
		"outcome: " + html.EscapeString(string(link.Outcome)), "iterations: " + strconv.Itoa(iterations),
		"result:", html.EscapeString(result), "</subagent_completion>",
	}, "\n")
}

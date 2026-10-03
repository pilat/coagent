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

func (s *store) Create(ctx context.Context, create Create) (int64, error) {
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
			return fmt.Errorf("create: %w", err)
		}

		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO subagent_links
   (parent_id,child_id,task_call_id,blocking,depth,state,created_at) VALUES (?,?,?,?,?,?,?)`,
			create.ParentID,
			childID,
			create.TaskCallID,
			create.Blocking,
			create.Depth,
			create.State,
			time.Now().UTC().Unix(),
		)
		if err != nil {
			return fmt.Errorf("insert subagent link: %w", err)
		}

		if create.InitialInput != "" {
			_, err = sessionstore.EnqueueTx(
				ctx,
				tx,
				sessionstore.Input{
					SessionID: childID,
					Source:    sessionstore.InputSourceAgent,
					Content:   create.InitialInput,
				},
			)
		}

		if err != nil {
			return fmt.Errorf("create: %w", err)
		}

		return nil
	})
	if err != nil {
		return childID, fmt.Errorf("create: %w", err)
	}

	return childID, nil
}

// Finalize commits the terminal link and session status together, preserving pending activations.
func (s *store) Finalize(ctx context.Context, childID int64, errored bool) (*Link, error) {
	var finalized *Link

	link, err := s.GetLink(ctx, childID)
	if err != nil {
		return link, err
	}

	if link == nil || link.Terminal() || link.State == StateStopped {
		return finalized, nil
	}

	record, err := s.sessions.GetSession(ctx, childID)
	if err != nil {
		return link, fmt.Errorf("load child session: %w", err)
	}

	if record.Status == sessionstore.SessionStatusSuspended && !errored {
		return finalized, nil
	}

	state := StateCompleted
	status := sessionstore.SessionStatusCompleted

	if (errored || record.Status == sessionstore.SessionStatusError) &&
		record.EmptyStopStreak < sessionstore.EmptyStopTerminalStreak {
		state = StateError
		status = sessionstore.SessionStatusError
	}

	result, outcome := s.deriveOutcome(ctx, childID, record.Iteration, errored,
		record.Status == sessionstore.SessionStatusError, record.EmptyStopStreak)

	err = s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		execResult, err := tx.ExecContext(ctx, `UPDATE subagent_links
   SET state = ?, result = ?, outcome = ?
   WHERE child_id = ? AND state IN ('spawned', 'running')
    AND EXISTS (SELECT 1 FROM sessions sess
     WHERE sess.id = subagent_links.child_id
      AND sess.status NOT IN ('stopping', 'stopped', 'killed') AND sess.killed_at IS NULL)
    AND NOT EXISTS (SELECT 1 FROM session_inbox input
     WHERE input.session_id = subagent_links.child_id AND input.state = 'pending')`,
			state, result, outcome, childID)
		if err != nil {
			return fmt.Errorf("conditionally finalize subagent activation: %w", err)
		}

		rows, err := execResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("finalize subagent activation rows affected: %w", err)
		}

		if rows == 0 {
			return nil
		}

		if err := sessionstore.UpdateSessionStatusTx(ctx, tx, childID, status); err != nil {
			return fmt.Errorf("finalize child status: %w", err)
		}

		finalized = link

		return nil
	})
	if err != nil {
		return link, fmt.Errorf("finalize child: %w", err)
	}

	if finalized != nil {
		finalized.State = state
		finalized.Result = result
		finalized.Outcome = outcome
	}

	return finalized, nil
}

// Kill terminalizes the link before killing its session in the same transaction.
func (s *store) Kill(ctx context.Context, childID int64) error {
	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE subagent_links SET state='killed', result='', outcome='killed' WHERE child_id = ?`,
			childID,
		)
		if err != nil {
			return fmt.Errorf("kill subagent link: %w", err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("killed link rows affected: %w", err)
		}

		if rows == 0 {
			return fmt.Errorf("subagent link for child %d not found", childID)
		}

		if err := sessionstore.MarkSessionKilledTx(ctx, tx, childID); err != nil {
			return fmt.Errorf("kill child session: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("kill child: %w", err)
	}

	return nil
}

// Resume starts a new background activation and updates its session atomically.
func (s *store) Resume(ctx context.Context, childID int64) error {
	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE subagent_links
   SET state = 'running', blocking = 0, activation_seq = activation_seq + 1,
    delivered_at = NULL, delivered_msg_id = NULL, delivered_input_id = NULL WHERE child_id = ?`, childID)
		if err != nil {
			return fmt.Errorf("reset link running: %w", err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("link rows affected: %w", err)
		}

		if rows == 0 {
			return fmt.Errorf("subagent link for child %d not found", childID)
		}

		if err := sessionstore.UpdateSessionStatusTx(ctx, tx, childID, sessionstore.SessionStatusActive); err != nil {
			return fmt.Errorf("resume child session: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("resume child: %w", err)
	}

	return nil
}

// Rearm activates delivered children with eligible pending input atomically.
func (s *store) Rearm(ctx context.Context, childID int64) (bool, error) {
	var rearmed bool

	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE subagent_links
   SET state = 'running', blocking = 0, activation_seq = activation_seq + 1,
    delivered_at = NULL, delivered_msg_id = NULL, delivered_input_id = NULL
   WHERE child_id = ? AND delivered_at IS NOT NULL
    AND ((state = 'completed' AND EXISTS (SELECT 1 FROM session_inbox input
     WHERE input.session_id = subagent_links.child_id AND input.state = 'pending'))
    OR (state = 'error' AND EXISTS (SELECT 1 FROM session_inbox input
     WHERE input.session_id = subagent_links.child_id AND input.state = 'pending'
      AND input.source IN ('user', 'agent'))))`, childID)
		if err != nil {
			return fmt.Errorf("rearm delivered subagent activation: %w", err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rearm subagent activation rows affected: %w", err)
		}

		rearmed = rows == 1
		if !rearmed {
			return nil
		}

		if err := sessionstore.UpdateSessionStatusTx(ctx, tx, childID, sessionstore.SessionStatusActive); err != nil {
			return fmt.Errorf("rearm child status: %w", err)
		}

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("rearm child: %w", err)
	}

	return rearmed, nil
}

func (s *store) DeliverBackgroundCompletion(ctx context.Context, link Link, iterations int) (bool, error) {
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

func (s *store) DeliverCompletion(ctx context.Context, link Link, content string) (bool, error) {
	if !link.Blocking {
		return false, fmt.Errorf("blocking delivery for background child %d", link.ChildID)
	}

	return s.deliver(ctx, link, sessionstore.Input{
		SessionID: link.ParentID, Source: sessionstore.InputSourceCallResult, Content: content,
		Attributes:  map[string]any{"call_id": link.TaskCallID, "tool_id": tool.IDTask},
		DeliveryKey: fmt.Sprintf("task:%d:%d", link.ChildID, link.ActivationSeq),
	})
}

func (s *store) deliver(ctx context.Context, link Link, input sessionstore.Input) (bool, error) {
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
				return fmt.Errorf("deliver: %w", err)
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

		if err != nil {
			return fmt.Errorf("deliver: %w", err)
		}

		return nil
	})
	if err != nil {
		return won, fmt.Errorf("deliver: %w", err)
	}

	return won, nil
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

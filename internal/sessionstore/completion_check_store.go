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

// ResponseDispositionKind names the single transition an accepted assistant
// response commits. Each kind carries its own completion-check, empty-streak,
// nudge, and output composition; the transaction commits exactly one of them.
type ResponseDispositionKind string

const (
	// ResponseDispositionCandidate stores a hidden no-wake final candidate and
	// its host nudge. No manager output is emitted.
	ResponseDispositionCandidate ResponseDispositionKind = "candidate"
	// ResponseDispositionConfirmed publishes the deliberate second stop as the
	// releasing final answer.
	ResponseDispositionConfirmed ResponseDispositionKind = "confirmed"
	// ResponseDispositionBackgroundYield publishes a no-wake stop through the
	// ordinary completion path because a durable wake source owns the next turn.
	ResponseDispositionBackgroundYield ResponseDispositionKind = "background_yield"
	// ResponseDispositionToolCall persists a tool-bearing response and clears
	// any pending check before execution.
	ResponseDispositionToolCall ResponseDispositionKind = "tool_call"
	// ResponseDispositionEmptyStop records one empty no-wake response and its
	// streak transition; the sixth commits the terminal host notice instead.
	ResponseDispositionEmptyStop ResponseDispositionKind = "empty_stop"
	// ResponseDispositionProjectionError retains the paid attempt and usage as
	// hidden evidence and commits the existing durable error outcome when the
	// wake-source projection fails.
	ResponseDispositionProjectionError ResponseDispositionKind = "projection_error"
)

// AcceptedResponseDisposition is the complete chosen transition for one
// accepted assistant response. Message is the assistant attempt; Nudge is the
// typed host completion nudge (candidate kind only); Output carries the
// pre-rendered final text (confirmed/yield/terminal kinds only).
type AcceptedResponseDisposition struct {
	SessionID  int64
	RootID     int64
	Iteration  int
	Message    *transcript.Message
	Kind       ResponseDispositionKind
	Nudge      *transcript.Message
	Output     string
	OutputType OutputType
	// ExpectedCandidateID validates confirm/clear transitions against the
	// durable check. Zero means the caller expects no pending check.
	ExpectedCandidateID int64
	// EmptyStopStreak is the caller's durable trailing empty-stop count after
	// applying this response. The store stamps it; the loop owns the counting.
	EmptyStopStreak int
	// ManagerReplyPending carries the caller's durable reply-obligation
	// projection: true when a manager-owned input owns this turn.
	ManagerReplyPending bool
	// RenderFinal composes the outbox content from post-disposition progress
	// facts captured on this transaction, replacing the store-reading final
	// renderer. Nil keeps the caller's text verbatim.
	RenderFinal func(facts *ProgressFacts) string
	ObservedAt  time.Time
}

// AcceptedResponseResult reports the identities one disposition commit created.
type AcceptedResponseResult struct {
	MessageID       int64
	NudgeMessageID  int64
	Output          *OutputCommit
	Budget          *BudgetRecord
	BudgetFired     bool
	EmptyStopStreak int
	// TerminalCommitted reports a projection-error disposition that already
	// committed the session's error state: the caller must not persist a
	// second generic error.
	TerminalCommitted bool
}

// EmptyStopTerminalStreak is the trailing count at which the loop commits the
// terminal host notice instead of another nudge.
const EmptyStopTerminalStreak = 6

// EmptyStopTerminalNotice is the durable host notice committed for the sixth
// consecutive empty response. Sessionlifecycle reuses it as a child's
// recovered result instead of fabricating a model-authored answer.
func EmptyStopTerminalNotice(count int) string {
	return fmt.Sprintf(
		"⚠️ Model returned %d consecutive empty responses. Session paused — waiting for input.",
		count,
	)
}

// ResponseDispositionStore commits accepted assistant responses with their
// iteration, completion-check, empty-streak, budget, nudge, and output effects.
type ResponseDispositionStore interface {
	CommitAcceptedResponseDisposition(
		ctx context.Context,
		disposition AcceptedResponseDisposition,
	) (*AcceptedResponseResult, error)
	// LoadCompletionCheckState reads the durable check, reply obligation, and
	// empty streak one loop turn resumes from.
	LoadCompletionCheckState(ctx context.Context, sessionID int64) (*CompletionCheckState, error)
}

// CompletionCheckState is the durable projection a loop turn resumes from.
type CompletionCheckState struct {
	CandidateID *int64
	// CandidateText resolves CandidateID's messages.content in the same read,
	// so a confirming disposition can publish the candidate instead of the
	// nudge ack. Empty when no candidate is pending.
	CandidateText       string
	ManagerReplyPending bool
	EmptyStopStreak     int
}

var (
	_ ResponseDispositionStore = (*store)(nil)
	// ErrCompletionCheckConflict reports a stale or mismatched candidate
	// identity: the caller's check no longer owns the durable transition.
	ErrCompletionCheckConflict = errors.New("completion check candidate conflict")
)

func (s *store) CommitAcceptedResponseDisposition(
	ctx context.Context,
	disposition AcceptedResponseDisposition,
) (*AcceptedResponseResult, error) {
	if err := validateAcceptedDisposition(disposition); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin accepted response disposition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := disposition.ObservedAt.UTC()
	if disposition.ObservedAt.IsZero() {
		now = time.Now().UTC()
	}

	messageID, err := insertMessageWith(ctx, tx, disposition.SessionID, disposition.Message)
	if err != nil {
		return nil, err
	}

	result := &AcceptedResponseResult{MessageID: messageID}

	if err := applyDispositionState(ctx, tx, disposition, messageID, now, result); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit accepted response disposition: %w", err)
	}

	return result, nil
}

func validateAcceptedDisposition(disposition AcceptedResponseDisposition) error {
	if disposition.SessionID <= 0 || disposition.RootID <= 0 || disposition.Iteration < 0 ||
		disposition.Message == nil || disposition.Message.Role != assistantRole {
		return errors.New("invalid accepted response disposition")
	}

	switch disposition.Kind {
	case ResponseDispositionCandidate:
		if disposition.Nudge == nil || disposition.Nudge.Role != userRole || disposition.Output != "" {
			return errors.New("candidate disposition requires a host nudge and no output")
		}
	case ResponseDispositionConfirmed, ResponseDispositionBackgroundYield:
		// A child or an output-disabled session confirms through the
		// transcript alone, so an empty composed output is legitimate.
	case ResponseDispositionToolCall:
		if !storedMessageHasToolCalls(disposition.Message) {
			return errors.New("tool disposition requires tool calls")
		}
	case ResponseDispositionEmptyStop:
		if disposition.Output != "" && disposition.Nudge != nil {
			return errors.New("terminal empty-stop disposition carries a notice, not a nudge")
		}

		if disposition.Output == "" &&
			(disposition.Nudge == nil || disposition.Nudge.Role != userRole) {
			return errors.New("empty-stop disposition requires a host nudge or terminal notice")
		}
	case ResponseDispositionProjectionError:
	default:
		return fmt.Errorf("unknown response disposition kind %q", disposition.Kind)
	}

	return nil
}

func applyDispositionState(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	messageID int64,
	now time.Time,
	result *AcceptedResponseResult,
) error {
	switch disposition.Kind {
	case ResponseDispositionCandidate:
		return applyCandidateDisposition(ctx, tx, disposition, messageID, now, result)
	case ResponseDispositionConfirmed:
		if err := setCompletionCandidate(
			ctx, tx, disposition.SessionID, disposition.ExpectedCandidateID, 0, now,
		); err != nil {
			return err
		}

		// The confirmed answer pointer outlives the check: a finalizing child
		// resolves it to the candidate's full text instead of the ack. Cleared
		// again by the next external model-visible input.
		if disposition.ExpectedCandidateID != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions
				SET completion_check_confirmed_answer_id = ?
				WHERE id = ? AND completion_check_candidate_id IS NULL`,
				disposition.ExpectedCandidateID, disposition.SessionID); err != nil {
				return fmt.Errorf("set confirmed answer pointer: %w", err)
			}
		}

		return commitDispositionOutput(ctx, tx, disposition, messageID, now, result, true)
	case ResponseDispositionBackgroundYield:
		return commitDispositionOutput(ctx, tx, disposition, messageID, now, result, true)
	case ResponseDispositionToolCall:
		if err := setCompletionCandidate(
			ctx, tx, disposition.SessionID, disposition.ExpectedCandidateID, 0, now,
		); err != nil {
			return err
		}

		// A direct reply for a tool-bearing response rides the same commit;
		// it never releases the input and never carries a final footer.
		if disposition.Output != "" {
			return commitDispositionOutput(ctx, tx, disposition, messageID, now, result, false)
		}

		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	case ResponseDispositionEmptyStop:
		// A terminal streak commits its durable host notice here: one
		// idempotent outbox row keyed to the attempt, never a re-notify.
		if disposition.Output != "" {
			return commitDispositionOutput(ctx, tx, disposition, messageID, now, result, true)
		}

		// The empty-stop nudge is model-visible evidence of this attempt; it
		// commits in the same transaction as the streak increment.
		if disposition.Nudge != nil {
			nudgeID, nudgeErr := insertMessageWith(ctx, tx, disposition.SessionID, disposition.Nudge)
			if nudgeErr != nil {
				return nudgeErr
			}

			result.NudgeMessageID = nudgeID
		}

		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	case ResponseDispositionProjectionError:
		return commitDispositionProjectionError(ctx, tx, disposition, messageID, now, result)
	default:
		return fmt.Errorf("unknown response disposition kind %q", disposition.Kind)
	}
}

// applyCandidateDisposition commits the hidden first candidate. A budget
// crossing on this attempt drops it with the suppressed answer, skips the
// nudge, and publishes only the host checkpoint.
func applyCandidateDisposition(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	messageID int64,
	now time.Time,
	result *AcceptedResponseResult,
) error {
	record, output, suppressed, obsErr := commitDispositionBudgetObservation(ctx, tx, disposition, now)
	if obsErr != nil {
		return obsErr
	}

	result.Budget = record

	// A fired budget suppresses this attempt's own answer: leaving the check
	// pending would resurface the text on the card note and as a later confirm.
	next := messageID
	if suppressed {
		next = 0
	}

	if err := setCompletionCandidate(ctx, tx, disposition.SessionID, 0, next, now); err != nil {
		return err
	}

	if suppressed {
		result.BudgetFired = true
		result.Output = output

		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	}

	nudgeID, nudgeErr := insertMessageWith(ctx, tx, disposition.SessionID, disposition.Nudge)
	if nudgeErr != nil {
		return nudgeErr
	}

	result.NudgeMessageID = nudgeID

	return updateDispositionIteration(
		ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
	)
}

// setCompletionCandidate validates the expected durable check and moves it to
// the next identity. The compare-and-set is one statement: concurrent
// dispositions with the same expected identity cannot both win, and a
// mismatch is a persistence conflict, never a silent adoption of an
// unrelated stop.
func setCompletionCandidate(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, expected, next int64,
	now time.Time,
) error {
	var result sql.Result

	var err error

	// next == 0 means "clear the check", which the column encodes as NULL.
	nextValue := any(next)
	if next == 0 {
		nextValue = nil
	}

	where := "completion_check_candidate_id IS NULL"
	args := []any{nextValue, now, sessionID}

	if expected != 0 {
		where = "completion_check_candidate_id = ?"

		args = append(args, expected)
	}

	result, err = tx.ExecContext(ctx, `UPDATE sessions
		SET completion_check_candidate_id = ?, updated_at = ?
		WHERE id = ? AND `+where, args...)
	if err != nil {
		return fmt.Errorf("update completion check candidate: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completion check update: %w", err)
	}

	if affected == 0 {
		var current sql.NullInt64

		if scanErr := tx.QueryRowContext(ctx, `SELECT completion_check_candidate_id
			FROM sessions WHERE id = ?`, sessionID).Scan(&current); scanErr != nil {
			return fmt.Errorf("load completion check state: %w", scanErr)
		}

		return fmt.Errorf("%w: session %d (expected %d, current %d valid %t)",
			ErrCompletionCheckConflict, sessionID, expected, current.Int64, current.Valid)
	}

	return requireOneSessionUpdate(result, sessionID)
}

// updateDispositionIteration advances the iteration, stamps the empty streak,
// and preserves the manager reply obligation carried by the caller.
func updateDispositionIteration(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	streak int,
	terminal bool,
	now time.Time,
	result *AcceptedResponseResult,
) error {
	query := `UPDATE sessions SET iteration = ?, empty_stop_streak = ?,
		manager_reply_pending = ?, updated_at = ?
		WHERE id = ? AND killed_at IS NULL AND status NOT IN ('stopping', 'terminating', 'killed')`
	args := []any{disposition.Iteration, streak, disposition.ManagerReplyPending, now, disposition.SessionID}

	if terminal {
		query = `UPDATE sessions SET iteration = ?, empty_stop_streak = ?,
			manager_reply_pending = ?, status = 'error', updated_at = ?
			WHERE id = ? AND killed_at IS NULL AND status NOT IN ('stopping', 'terminating', 'killed')`
	}

	executed, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update disposition iteration: %w", err)
	}

	if err := requireOneSessionUpdate(executed, disposition.SessionID); err != nil {
		return err
	}

	result.EmptyStopStreak = streak

	return nil
}

// commitDispositionOutput composes budget observation with the optional manager
// output: a budget crossing suppresses the model text and publishes only the
// host checkpoint. Releasing outputs clear the reply obligation atomically.
func commitDispositionOutput(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	messageID int64,
	now time.Time,
	result *AcceptedResponseResult,
	releasing bool,
) error {
	record, output, suppressed, err := commitDispositionBudgetObservation(ctx, tx, disposition, now)
	if err != nil {
		return err
	}

	result.Budget = record
	result.Output = output

	if suppressed {
		result.BudgetFired = true

		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	}

	if disposition.Output == "" {
		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	}

	owner, err := outputOwner(ctx, tx, disposition.SessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		return updateDispositionIteration(
			ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result,
		)
	}

	if err != nil {
		return err
	}

	if err := outputSessionWritable(ctx, tx, disposition.SessionID); err != nil {
		return err
	}

	// The iteration advances before the outbox insert so the captured facts
	// describe the post-disposition state the committed output claims.
	preserved := disposition
	if releasing {
		preserved.ManagerReplyPending = false
	}

	if err := updateDispositionIteration(
		ctx, tx, preserved, preserved.EmptyStopStreak, false, now, result,
	); err != nil {
		return err
	}

	commit, err := insertDispositionOutput(
		ctx, tx, disposition, owner, messageID, now, releasing,
	)
	if err != nil {
		return err
	}

	result.Output = commit

	return nil
}

func insertDispositionOutput(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	owner string,
	messageID int64,
	now time.Time,
	releasing bool,
) (*OutputCommit, error) {
	outputType := disposition.OutputType
	if outputType == "" {
		outputType = OutputMessagePersistent
	}

	if outputType != OutputMessagePersistent && outputType != OutputMessageReplaceable {
		return nil, errors.New("invalid disposition output type")
	}

	phase := outputPhaseProgress
	if releasing {
		phase = outputPhaseFinal
	} else if outputType == OutputMessagePersistent {
		phase = outputPhaseReply
	}

	content := disposition.Output
	if disposition.RenderFinal != nil && phase == outputPhaseFinal {
		facts, err := CaptureProgressTx(ctx, tx, disposition.RootID)
		if err != nil {
			return nil, fmt.Errorf("capture disposition final facts: %w", err)
		}

		content = disposition.RenderFinal(facts)
	}

	key := fmt.Sprintf("message:%d:%s", messageID, phase)
	fingerprint := outputFingerprintWithRelease(outputType, content, disposition.SessionID, nil, releasing)

	attributes, err := stampMessageOutputAttributes(ctx, tx, disposition.SessionID, owner, nil)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal disposition output attributes: %w", err)
	}

	executed, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		disposition.SessionID, outputType, content, string(encoded),
		key, fingerprint, now, releasing)
	if err != nil {
		return nil, fmt.Errorf("insert disposition output: %w", err)
	}

	outputID, err := executed.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("disposition output id: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

// commitDispositionBudgetObservation mirrors the rejected-response budget
// precedence: an already-fired budget suppresses, an armed crossing fires.
func commitDispositionBudgetObservation(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	now time.Time,
) (*BudgetRecord, *OutputCommit, bool, error) {
	record, err := scanBudget(
		tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, disposition.RootID),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}

	if err != nil {
		return nil, nil, false, fmt.Errorf("load disposition budget: %w", err)
	}

	if record.State == BudgetFired {
		return record, nil, true, nil
	}

	if record.State != BudgetArmed {
		return record, nil, false, nil
	}

	reason, delta, err := budgetCrossing(ctx, tx, record, now)
	if err != nil || reason == "" {
		return record, nil, false, err
	}

	output, err := fireDispositionBudget(ctx, tx, disposition, record, reason, delta, now)
	if err != nil {
		return nil, nil, false, err
	}

	return record, output, true, nil
}

func fireDispositionBudget(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	record *BudgetRecord,
	reason string,
	delta float64,
	now time.Time,
) (*OutputCommit, error) {
	parkOwner := fmt.Sprintf("budget:%d:%d", disposition.RootID, record.Generation)

	fired, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'fired', fired_at = ?,
		fired_reason = ?, observed_cost_usd = ?, park_phase = 'requested', park_owner = ?
		WHERE root_session_id = ? AND generation = ? AND state = 'armed'`,
		now, reason, delta, parkOwner, disposition.RootID, record.Generation)
	if err != nil {
		return nil, fmt.Errorf("fire disposition budget: %w", err)
	}

	if err := requireActivationChanged(fired); err != nil {
		return nil, ErrBudgetConflict
	}

	owner, err := outputOwner(ctx, tx, disposition.RootID)
	if err != nil {
		return nil, err
	}

	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)

	output, err := insertMessageOutput(ctx, tx, disposition.RootID, owner, content,
		fmt.Sprintf("budget:%d:checkpoint", record.Generation), now, true)
	if err != nil {
		return nil, err
	}

	record.State = BudgetFired
	record.FiredReason = reason
	record.FiredAt = &now
	record.ObservedCostUSD = &delta
	record.ParkPhase = budgetParkRequestedState
	record.ParkOwner = parkOwner

	return output, nil
}

// commitDispositionProjectionError retains the paid attempt and usage as hidden
// evidence and commits the existing durable error outcome plus one host error
// output for a root. Budget precedence applies first.
func commitDispositionProjectionError(
	ctx context.Context,
	tx *sql.Tx,
	disposition AcceptedResponseDisposition,
	messageID int64,
	now time.Time,
	result *AcceptedResponseResult,
) error {
	_, output, suppressed, err := commitDispositionBudgetObservation(ctx, tx, disposition, now)
	if err != nil {
		return err
	}

	result.BudgetFired = suppressed
	if suppressed {
		result.Output = output

		return updateDispositionIteration(ctx, tx, disposition, disposition.EmptyStopStreak, false, now, result)
	}

	_ = messageID

	if err := updateDispositionIteration(
		ctx, tx, disposition, disposition.EmptyStopStreak, true, now, result,
	); err != nil {
		return err
	}

	owner, err := outputOwner(ctx, tx, disposition.SessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		result.TerminalCommitted = true

		return nil
	}

	if err != nil {
		return err
	}

	commit, err := insertMessageOutput(
		ctx, tx, disposition.SessionID, owner, disposition.Output,
		fmt.Sprintf("projection-error:%d:terminal", disposition.Iteration), now, true,
	)
	if err != nil {
		return err
	}

	preserved := disposition
	preserved.ManagerReplyPending = false

	if err := updateDispositionIteration(
		ctx, tx, preserved, preserved.EmptyStopStreak, true, now, result,
	); err != nil {
		return err
	}

	result.Output = commit
	result.TerminalCommitted = true

	return nil
}

// LoadCompletionCheckState reads the durable check, reply obligation, and
// empty streak one loop turn resumes from, resolving the pending candidate's
// text in the same query.
func (s *store) LoadCompletionCheckState(ctx context.Context, sessionID int64) (*CompletionCheckState, error) {
	var candidate sql.NullInt64
	var candidateText sql.NullString
	var replyPending sql.NullBool
	var streak sql.NullInt64

	err := s.db.QueryRowContext(ctx, `SELECT sessions.completion_check_candidate_id,
		messages.content, sessions.manager_reply_pending, sessions.empty_stop_streak
		FROM sessions LEFT JOIN messages ON messages.id = sessions.completion_check_candidate_id
		WHERE sessions.id = ?`, sessionID).
		Scan(&candidate, &candidateText, &replyPending, &streak)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSessionNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load completion check state: %w", err)
	}

	state := &CompletionCheckState{
		CandidateText:       candidateText.String,
		ManagerReplyPending: replyPending.Bool,
		EmptyStopStreak:     int(streak.Int64),
	}
	if candidate.Valid {
		state.CandidateID = &candidate.Int64
	}

	return state, nil
}

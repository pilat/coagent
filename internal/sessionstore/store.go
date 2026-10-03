package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/transcript"
)

const (
	defaultReasoningLevel = "medium"
	assistantRole         = "assistant"
	userRole              = "user"
)

// Written explicitly on every root session: the column's schema default is
// 'general', a subagent type that strips the primary agent's todo tools.
const rootAgentType = "build"

const (
	SessionStatusActive      SessionStatus = "active"
	SessionStatusCompleted   SessionStatus = "completed"
	SessionStatusSuspended   SessionStatus = "suspended"
	SessionStatusError       SessionStatus = "error"
	SessionStatusStopping    SessionStatus = "stopping"
	SessionStatusStopped     SessionStatus = "stopped"
	SessionStatusTerminating SessionStatus = "terminating"
	SessionStatusKilled      SessionStatus = "killed"
)

const sessionColumns = `id, project_id, model, reasoning_level, master_enabled, attributes, agent_type, parent_id, iteration, status, todo_items, created_at, updated_at, killed_at, root_id, model_input_generation, model_input_boundary, context_baseline_model, context_baseline_prompt_tokens, context_baseline_message_count, completion_check_candidate_id, manager_reply_pending, empty_stop_streak, completion_check_confirmed_answer_id`

// errSessionNotFound signals a lookup query matched no row.
var errSessionNotFound = errors.New("session not found")

// SessionStatus is the persisted sessions.status vocabulary. It is deliberately
// distinct from controller runtime state and daemon subagent-link state.
type SessionStatus string

// SessionRecord represents a row in the sessions table.
type SessionRecord struct {
	ID             int64
	ProjectID      int64
	Model          string
	ReasoningLevel string
	MasterEnabled  bool
	Attributes     map[string]any
	AgentType      string
	ParentID       int64
	RootID         int64
	Iteration      int
	Status         SessionStatus
	TodoItems      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	KilledAt       *time.Time
	// ModelInputGeneration is the monotonic generation advanced only when
	// model-bound input enters conversation history; zero means no generated
	// input has ever been committed for this session.
	ModelInputGeneration int64
	// ModelInputBoundary is the transcript message ID at which the current
	// generation began. Nil only on sessions with generation 0 and no history.
	ModelInputBoundary *int64

	// Persisted context baseline: the last provider-measured prompt size and
	// the transcript length it covered. Zero values mean nothing was measured;
	// the model column guards the measurement against model switches.
	ContextBaselineModel        string
	ContextBaselinePromptTokens int
	ContextBaselineMessageCount int

	// CompletionCheckCandidateID points at the hidden final-candidate assistant
	// message awaiting its deliberate second stop. Nil means no check pending.
	CompletionCheckCandidateID *int64
	// CompletionCheckConfirmedAnswerID points at the candidate row the last
	// confirmed check published: a finalizing child resolves it to the full
	// answer instead of the ack. Nil when no confirmed answer is outstanding.
	CompletionCheckConfirmedAnswerID *int64
	// ManagerReplyPending is the durable manager-reply obligation: set when a
	// manager-owned model input is promoted, cleared only by a releasing output
	// or a terminal settlement that supersedes the turn.
	ManagerReplyPending bool
	// EmptyStopStreak is the durable trailing count of empty no-wake stop
	// responses feeding loop detection's 3/6 escalation.
	EmptyStopStreak int
}

// CompactionEntry is either an existing active row or a new message in rebuilt transcript order.
type CompactionEntry struct {
	ExistingID int64
	Message    *transcript.Message
}

// Store owns session, input, output and project persistence.
type Store struct {
	db            *sql.DB
	wokenMu       sync.Mutex
	wokenSessions map[int64]struct{}
	woken         chan struct{}
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

type scannedMessageValues struct {
	toolCallID, toolName, toolCallsRaw, reasoningContent, reasoningRaw sql.NullString
	attachmentsRaw, usageRaw, finishType, providerFinishReason         sql.NullString
	rejectedReason                                                     sql.NullString
	retryOfMessageID                                                   sql.NullInt64
	compactedAt                                                        sql.NullTime
	costUSD                                                            sql.NullFloat64
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, wokenSessions: make(map[int64]struct{}), woken: make(chan struct{}, 1)}
}

func (s *Store) CreateSession(
	ctx context.Context,
	projectID int64,
	model, reasoningLevel string,
	attrs map[string]any,
) (*SessionRecord, error) {
	if reasoningLevel == "" {
		reasoningLevel = defaultReasoningLevel
	}

	if attrs == nil {
		attrs = map[string]any{}
	}

	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("marshal attributes: %w", err)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO sessions (project_id, model, reasoning_level, attributes, agent_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		projectID,
		model,
		reasoningLevel,
		string(attrsJSON),
		rootAgentType,
		now,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	newID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}

	rec := &SessionRecord{
		ID:             newID,
		ProjectID:      projectID,
		Model:          model,
		ReasoningLevel: reasoningLevel,
		Status:         SessionStatusActive,
		AgentType:      rootAgentType,
		Attributes:     attrs,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	return rec, nil
}

func (s *Store) CreateSubagentSession(
	ctx context.Context,
	projectID, parentID, rootID int64,
	agentType, model, reasoningLevel string,
) (int64, error) {
	if reasoningLevel == "" {
		reasoningLevel = defaultReasoningLevel
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO sessions
			(project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ? FROM sessions WHERE id = ?`,
		projectID,
		parentID,
		rootID,
		agentType,
		model,
		reasoningLevel,
		now,
		now,
		parentID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert subagent session: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check parent session inheritance: %w", err)
	}

	if rows == 0 {
		return 0, fmt.Errorf("parent session %d not found", parentID)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	return id, nil
}

//nolint:wsl_v5 // Fencing and replacement creation must remain one transaction.
func (s *Store) CreateReplacementSession(
	ctx context.Context,
	oldSessionID int64,
) (*SessionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ownerless replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	old, err := scanSession(
		tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, oldSessionID),
	)
	if err != nil {
		return nil, fmt.Errorf("load ownerless replacement: %w", err)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'terminating', updated_at = ?
		WHERE id = ? AND parent_id = 0 AND killed_at IS NULL
			AND status NOT IN ('terminating', 'killed')`, now, oldSessionID)
	if err != nil {
		return nil, fmt.Errorf("fence ownerless replacement: %w", err)
	}
	if err := requireOneSessionUpdate(result, oldSessionID); err != nil {
		return nil, err
	}

	attrs, err := json.Marshal(old.Attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal ownerless replacement attributes: %w", err)
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO sessions
		(project_id, model, reasoning_level, attributes, agent_type, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		old.ProjectID, old.Model, old.ReasoningLevel, string(attrs), rootAgentType, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert ownerless replacement: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("ownerless replacement id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ownerless replacement: %w", err)
	}

	return &SessionRecord{
		ID: id, ProjectID: old.ProjectID, Model: old.Model, ReasoningLevel: old.ReasoningLevel,
		Status: SessionStatusActive, AgentType: rootAgentType, Attributes: old.Attributes,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) SetAttributes(ctx context.Context, id int64, attrs map[string]any) error {
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("marshal attributes: %w", err)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`UPDATE sessions SET attributes = ?, updated_at = ? WHERE id = ?`,
		string(attrsJSON), now, id,
	)
	if err != nil {
		return fmt.Errorf("set attributes: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", id)
	}

	return nil
}

func (s *Store) UpdateSessionModel(ctx context.Context, id int64, model, reasoningLevel string) error {
	now := time.Now().UTC()

	// Written verbatim: a model that offers no effort choice legitimately carries
	// an empty level, and the caller has already settled it against the catalog.
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE sessions SET model = ?, reasoning_level = ?, updated_at = ? WHERE id = ?`,
		model, reasoningLevel, now, id,
	)
	if err != nil {
		return fmt.Errorf("update session model: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", id)
	}

	return nil
}

func (s *Store) GetSession(ctx context.Context, id int64) (*SessionRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id,
	)

	return scanSession(row)
}

func (s *Store) ListSessions(ctx context.Context) ([]*SessionRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE parent_id = 0 ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var records []*SessionRecord

	for rows.Next() {
		rec, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	return records, nil
}

// ListAllSessions is the daemon lifecycle view, including descendants. Public
// session listings intentionally use ListSessions, which returns roots only.
func (s *Store) ListAllSessions(ctx context.Context) ([]*SessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query all sessions: %w", err)
	}
	defer rows.Close()

	var records []*SessionRecord

	for rows.Next() {
		rec, scanErr := scanSessionRows(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate all sessions: %w", err)
	}

	return records, nil
}

func (s *Store) FindSessionByProjectID(ctx context.Context, projectID int64) (*SessionRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE project_id = ? AND parent_id = 0 AND killed_at IS NULL ORDER BY updated_at DESC LIMIT 1`,
		projectID,
	)

	rec, err := scanSession(row)
	if err != nil {
		return nil, err
	}

	return rec, nil
}

// LatestActivityByProject maps each project id to its most recent session
// updated_at, preferring non-killed sessions so that killing an old dialog (or
// the startup terminating-sweep) never floats a dead project to the top; a
// project whose sessions are all killed falls back to the newest of any. Projects
// with no sessions are absent from the map. The plain updated_at column is read
// via ORDER BY ... LIMIT 1 (not MAX()) so modernc.org/sqlite keeps the DATETIME
// decltype and scans straight into time.Time.
func (s *Store) LatestActivityByProject(ctx context.Context, projectIDs []int64) (map[int64]time.Time, error) {
	result := make(map[int64]time.Time, len(projectIDs))

	for _, pid := range projectIDs {
		t, ok, err := s.latestActivity(ctx, pid)
		if err != nil {
			return nil, err
		}

		if ok {
			result[pid] = t
		}
	}

	return result, nil
}

func (s *Store) MarkSessionKilled(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	var parentID int64
	if err := tx.QueryRowContext(ctx, `SELECT parent_id FROM sessions WHERE id = ?`, id).Scan(&parentID); err != nil {
		return fmt.Errorf("load session for kill: %w", err)
	}

	result, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'killed', killed_at = ?, updated_at = ? WHERE id = ?`,
		now,
		now,
		id,
	)
	if err != nil {
		return fmt.Errorf("mark session killed: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", id)
	}

	if err := cancelPendingInputTree(ctx, tx, id, parentID == 0, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id int64, status SessionStatus) error {
	if !status.valid() {
		return fmt.Errorf("invalid session status %q", status)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", id)
	}

	return nil
}

// KillTerminatingSessions finishes the boot reconciliation of roots left mid
// clear or kill: a matching replacement row means clear transferred the
// surface, its absence selects kill cleanup with a close output.
func (s *Store) KillTerminatingSessions(ctx context.Context) error {
	now := time.Now().UTC()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, attributes FROM sessions
		WHERE status = 'terminating' AND killed_at IS NULL`)
	if err != nil {
		return fmt.Errorf("list terminating sessions: %w", err)
	}
	defer rows.Close()

	type terminating struct {
		id    int64
		owner string
	}

	targets := make([]terminating, 0)

	for rows.Next() {
		var target terminating

		var encoded string
		if err := rows.Scan(&target.id, &encoded); err != nil {
			return fmt.Errorf("scan terminating session: %w", err)
		}

		var attributes map[string]any
		if err := json.Unmarshal([]byte(encoded), &attributes); err != nil {
			return fmt.Errorf("decode session %d attributes: %w", target.id, err)
		}

		target.owner, _ = attributes[managerIDAttribute].(string)
		targets = append(targets, target)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate terminating sessions: %w", err)
	}

	for _, target := range targets {
		if err := s.killTerminatingTarget(ctx, target.id, target.owner, now); err != nil {
			return err
		}
	}

	return nil
}

//nolint:nonamedreturns // two same-typed int results are ambiguous at call sites without names
func (s *Store) GetChildSessionStats(ctx context.Context, rootID int64) (count, totalIterations int, err error) {
	err = s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(iteration), 0) FROM sessions WHERE root_id = ?`,
		rootID,
	).
		Scan(&count, &totalIterations)
	if err != nil {
		return 0, 0, fmt.Errorf("get child session stats: %w", err)
	}

	return count, totalIterations, nil
}

//nolint:nonamedreturns // three heterogeneous results are ambiguous at call sites without names
func (s *Store) GetSessionTreeUsage(
	ctx context.Context,
	rootID int64,
) (promptTokens, completionTokens int, costUSD float64, err error) {
	// root_id lives on sessions, so join: s.id = rootID catches the root's own rows
	// (root_id defaults to 0), s.root_id = rootID catches descendants. No compacted filter.
	err = s.db.QueryRowContext(
		ctx,
		`SELECT
			COALESCE(SUM(json_extract(m.usage, '$.promptTokens')), 0),
			COALESCE(SUM(json_extract(m.usage, '$.completionTokens')), 0),
			COALESCE(SUM(m.cost_usd), 0)
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.id = ? OR s.root_id = ?`,
		rootID, rootID,
	).Scan(&promptTokens, &completionTokens, &costUSD)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get session tree usage: %w", err)
	}

	return promptTokens, completionTokens, costUSD, nil
}

// LoadMessageContentByID resolves one message's text regardless of compaction
// state: compaction only stamps compacted_at and never rewrites content, so a
// confirmed answer pointer stays resolvable for the session's whole life.
func (s *Store) LoadMessageContentByID(ctx context.Context, sessionID, messageID int64) (string, error) {
	var content sql.NullString

	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND id = ?`,
		sessionID, messageID,
	).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSessionNotFound
	}

	if err != nil {
		return "", fmt.Errorf("load message content by id: %w", err)
	}

	return content.String, nil
}

func (s *Store) LoadActiveMessages(ctx context.Context, sessionID int64) ([]*transcript.Message, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, session_id, role, content, tool_call_id, tool_name, tool_error, tool_calls,
			reasoning_content, reasoning_raw, attachments, cost_usd, usage, finish_type,
			provider_finish_reason, rejected_reason, retry_of_message_id, compacted_at, created_at
		FROM messages WHERE session_id = ? AND compacted_at IS NULL AND rejected_reason IS NULL
		ORDER BY position IS NULL, position, id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query active messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

func (s SessionStatus) valid() bool {
	switch s {
	case SessionStatusActive,
		SessionStatusCompleted,
		SessionStatusSuspended,
		SessionStatusError,
		SessionStatusStopping,
		SessionStatusStopped,
		SessionStatusTerminating,
		SessionStatusKilled:
		return true
	default:
		return false
	}
}

func insertMessageWith(ctx context.Context, q execer, sessionID int64, msg *transcript.Message) (int64, error) {
	if err := validateRetryReference(ctx, q, sessionID, msg.RetryOfMessageID); err != nil {
		return 0, err
	}

	result, err := q.ExecContext(
		ctx,
		`INSERT INTO messages (session_id, role, content, tool_call_id, tool_name, tool_error,
			tool_calls, reasoning_content, reasoning_raw, attachments, cost_usd, usage,
			finish_type, provider_finish_reason, rejected_reason, retry_of_message_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		msg.Role,
		msg.Content,
		nullString(msg.ToolCallID),
		nullString(msg.ToolName),
		msg.ToolError,
		nullRawJSON(msg.ToolCalls),
		msg.ReasoningContent,
		nullRawJSON(msg.ReasoningRaw),
		nullRawJSON(msg.Attachments),
		msg.CostUSD,
		nullRawJSON(msg.Usage),
		nullString(msg.FinishType),
		nullString(msg.ProviderFinishReason),
		nullString(msg.RejectedReason),
		nullMessageID(msg.RetryOfMessageID),
	)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	return id, nil
}

func validateRetryReference(ctx context.Context, q execer, sessionID, retryOfMessageID int64) error {
	if retryOfMessageID == 0 {
		return nil
	}

	var referencedSessionID int64
	var role string
	var rejectedReason sql.NullString

	err := q.QueryRowContext(ctx, `SELECT session_id, role, rejected_reason FROM messages WHERE id = ?`,
		retryOfMessageID).Scan(&referencedSessionID, &role, &rejectedReason)
	if err != nil {
		return fmt.Errorf("load retry attempt: %w", err)
	}

	if referencedSessionID != sessionID || role != assistantRole || !rejectedReason.Valid {
		return errors.New("retry message does not reference a rejected assistant attempt in the same session")
	}

	return nil
}

func replaceCompactedMessagesTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	compactedIDs []int64,
	entries []CompactionEntry,
	now time.Time,
) ([]int64, error) {
	for _, id := range compactedIDs {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE messages SET compacted_at = ? WHERE id = ? AND session_id = ?`,
			now,
			id,
			sessionID,
		)
		if err != nil {
			return nil, fmt.Errorf("mark compacted %d: %w", id, err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count compacted message %d: %w", id, err)
		}

		if rows != 1 {
			return nil, fmt.Errorf("compacted message %d does not belong to session %d", id, sessionID)
		}
	}

	ids := make([]int64, len(entries))
	for i, entry := range entries {
		position := i + 1

		if entry.ExistingID != 0 {
			result, err := tx.ExecContext(
				ctx,
				`UPDATE messages SET position = ? WHERE id = ? AND session_id = ?`,
				position,
				entry.ExistingID,
				sessionID,
			)
			if err != nil {
				return nil, fmt.Errorf("position existing message %d: %w", entry.ExistingID, err)
			}

			rows, err := result.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("position message rows affected: %w", err)
			}

			if rows != 1 {
				return nil, fmt.Errorf("existing message %d not found", entry.ExistingID)
			}

			ids[i] = entry.ExistingID

			continue
		}

		if entry.Message == nil {
			return nil, fmt.Errorf("compaction entry %d has no message", i)
		}

		id, err := insertMessageWith(ctx, tx, sessionID, entry.Message)
		if err != nil {
			return nil, fmt.Errorf("insert replacement message %d: %w", i, err)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE messages SET position = ? WHERE id = ?`, position, id); err != nil {
			return nil, fmt.Errorf("position replacement message %d: %w", id, err)
		}

		ids[i] = id
	}

	return ids, nil
}

func (s *Store) killTerminatingTarget(ctx context.Context, id int64, owner string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terminating kill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'killed', killed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'terminating' AND killed_at IS NULL`,
		now, now, id,
	)
	if err != nil {
		return fmt.Errorf("kill terminating sessions: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("kill terminating rows affected: %w", err)
	}

	if affected == 0 || owner == "" {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit terminating kill: %w", err)
		}

		return nil
	}

	replaced, err := hasReplacementRow(ctx, tx, id, owner)
	if err != nil {
		return err
	}

	if !replaced {
		if _, err := insertClosedOutput(ctx, tx, id, now, 0); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit terminating kill: %w", err)
	}

	return nil
}

func (s *Store) latestActivity(ctx context.Context, projectID int64) (time.Time, bool, error) {
	t, ok, err := s.maxUpdatedAt(ctx, projectID, true)
	if err != nil || ok {
		return t, ok, err
	}

	return s.maxUpdatedAt(ctx, projectID, false)
}

func (s *Store) maxUpdatedAt(ctx context.Context, projectID int64, excludeKilled bool) (time.Time, bool, error) {
	// parent_id = 0: rank a project by its top-level dialogs, not internal subagent
	// churn — mirrors FindSessionByProjectID.
	query := `SELECT updated_at FROM sessions WHERE project_id = ? AND parent_id = 0`
	if excludeKilled {
		query += ` AND killed_at IS NULL`
	}

	query += ` ORDER BY updated_at DESC LIMIT 1`

	var t time.Time

	err := s.db.QueryRowContext(ctx, query, projectID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}

	if err != nil {
		return time.Time{}, false, fmt.Errorf("latest activity for project %d: %w", projectID, err)
	}

	return t, true, nil
}

func scanSession(row *sql.Row) (*SessionRecord, error) {
	rec, err := scanSessionFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSessionNotFound
		}

		return nil, err
	}

	return rec, nil
}

func scanSessionRows(rows *sql.Rows) (*SessionRecord, error) {
	return scanSessionFrom(rows)
}

func scanSessionFrom(sc rowScanner) (*SessionRecord, error) {
	var rec SessionRecord
	var model, reasoning, attrsRaw sql.NullString
	var agentType, status, todoItems sql.NullString
	var masterEnabled sql.NullBool
	var projectID, parentID, iteration, rootID sql.NullInt64
	var killedAt sql.NullTime
	var boundary sql.NullInt64
	var candidateID sql.NullInt64
	var managerReplyPending sql.NullBool
	var emptyStopStreak sql.NullInt64
	var confirmedAnswerID sql.NullInt64

	err := sc.Scan(&rec.ID, &projectID, &model, &reasoning, &masterEnabled, &attrsRaw,
		&agentType, &parentID, &iteration, &status, &todoItems,
		&rec.CreatedAt, &rec.UpdatedAt, &killedAt, &rootID,
		&rec.ModelInputGeneration, &boundary,
		&rec.ContextBaselineModel, &rec.ContextBaselinePromptTokens, &rec.ContextBaselineMessageCount,
		&candidateID, &managerReplyPending, &emptyStopStreak, &confirmedAnswerID)
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	rec.ProjectID = projectID.Int64
	rec.Model = model.String
	rec.ReasoningLevel = reasoning.String

	rec.MasterEnabled = masterEnabled.Valid && masterEnabled.Bool
	rec.Attributes = unmarshalAttributes(attrsRaw.String)
	rec.AgentType = agentType.String
	rec.ParentID = parentID.Int64
	rec.RootID = rootID.Int64
	rec.Iteration = int(iteration.Int64)
	rec.Status = SessionStatus(status.String)
	rec.TodoItems = todoItems.String

	if killedAt.Valid {
		rec.KilledAt = &killedAt.Time
	}

	if boundary.Valid {
		rec.ModelInputBoundary = &boundary.Int64
	}

	if candidateID.Valid {
		rec.CompletionCheckCandidateID = &candidateID.Int64
	}

	if confirmedAnswerID.Valid {
		rec.CompletionCheckConfirmedAnswerID = &confirmedAnswerID.Int64
	}

	rec.ManagerReplyPending = managerReplyPending.Bool
	rec.EmptyStopStreak = int(emptyStopStreak.Int64)

	return &rec, nil
}

func scanMessages(rows *sql.Rows) ([]*transcript.Message, error) {
	var messages []*transcript.Message

	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}

		messages = append(messages, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
}

func scanMessage(sc rowScanner) (*transcript.Message, error) {
	var msg transcript.Message
	var values scannedMessageValues

	err := sc.Scan(
		&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
		&values.toolCallID, &values.toolName, &msg.ToolError, &values.toolCallsRaw,
		&values.reasoningContent, &values.reasoningRaw, &values.attachmentsRaw,
		&values.costUSD, &values.usageRaw, &values.finishType, &values.providerFinishReason,
		&values.rejectedReason, &values.retryOfMessageID, &values.compactedAt, &msg.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}

	values.apply(&msg)

	return &msg, nil
}

func (v scannedMessageValues) apply(msg *transcript.Message) {
	msg.ToolCallID = v.toolCallID.String
	msg.ToolName = v.toolName.String

	if v.toolCallsRaw.Valid && v.toolCallsRaw.String != "" {
		msg.ToolCalls = json.RawMessage(v.toolCallsRaw.String)
	}

	msg.ReasoningContent = v.reasoningContent.String

	if v.reasoningRaw.Valid && v.reasoningRaw.String != "" {
		msg.ReasoningRaw = json.RawMessage(v.reasoningRaw.String)
	}

	if v.attachmentsRaw.Valid && v.attachmentsRaw.String != "" {
		msg.Attachments = json.RawMessage(v.attachmentsRaw.String)
	}

	msg.CostUSD = v.costUSD.Float64

	if v.usageRaw.Valid && v.usageRaw.String != "" {
		msg.Usage = json.RawMessage(v.usageRaw.String)
	}

	msg.FinishType = v.finishType.String
	msg.ProviderFinishReason = v.providerFinishReason.String
	msg.RejectedReason = v.rejectedReason.String
	msg.RetryOfMessageID = v.retryOfMessageID.Int64

	if v.compactedAt.Valid {
		msg.CompactedAt = &v.compactedAt.Time
	}
}

func nullMessageID(value int64) any {
	if value == 0 {
		return nil
	}

	return value
}

func unmarshalAttributes(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}

	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		return map[string]any{}
	}

	return attrs
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}

func nullRawJSON(raw json.RawMessage) sql.NullString {
	if len(raw) == 0 || string(raw) == "null" {
		return sql.NullString{}
	}

	return sql.NullString{String: string(raw), Valid: true}
}

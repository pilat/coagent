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

const defaultReasoningLevel = "medium"

// Written explicitly on every root session: the column's schema default is
// 'general', a subagent type that strips the primary agent's todo tools.
const rootAgentType = "build"

// SessionStatus is the persisted sessions.status vocabulary. It is deliberately
// distinct from controller runtime state and daemon subagent-link state.
type SessionStatus string

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

const sessionColumns = `id, project_id, model, reasoning_level, master_enabled, attributes, agent_type, parent_id, iteration, status, todo_items, created_at, updated_at, killed_at, root_id, model_input_generation, model_input_boundary, context_baseline_model, context_baseline_prompt_tokens, context_baseline_message_count`

// errSessionNotFound signals a lookup query matched no row.
var errSessionNotFound = errors.New("session not found")

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
}

// StoredMessage is retained while transcript consumers migrate to its owner.
type StoredMessage = transcript.Message

// CompactionEntry is either an existing active row or a new message in rebuilt transcript order.
type CompactionEntry struct {
	ExistingID int64
	Message    *StoredMessage
}

// RuntimeStore is the persistence capability used by a live agent session. It
// deliberately excludes session creation, discovery and lifecycle orchestration:
// a session may checkpoint itself and mutate its transcript, but cannot create
// or kill another session.
type RuntimeStore interface {
	InsertMessage(ctx context.Context, sessionID int64, msg *StoredMessage) (int64, error)
	MarkCompacted(ctx context.Context, ids []int64) error
	ReplaceCompactedMessages(
		ctx context.Context,
		sessionID int64,
		compactedIDs []int64,
		entries []CompactionEntry,
	) ([]int64, error)
	LoadActiveMessages(ctx context.Context, sessionID int64) ([]*StoredMessage, error)

	UpdateSessionIteration(ctx context.Context, id int64, iteration int, status SessionStatus) error
	UpdateSessionTodoItems(ctx context.Context, id int64, items json.RawMessage) error
	// SaveContextBaseline persists the last provider-measured context size;
	// ClearContextBaseline drops it when the transcript it described is gone.
	SaveContextBaseline(ctx context.Context, sessionID int64, b ContextBaseline) error
	ClearContextBaseline(ctx context.Context, sessionID int64) error
	GetChildSessionStats(ctx context.Context, rootID int64) (count int, totalIterations int, err error)
	// GetSessionTreeUsage sums token usage and cost over the whole session tree
	// rooted at rootID, including compacted rows.
	GetSessionTreeUsage(
		ctx context.Context,
		rootID int64,
	) (promptTokens int, completionTokens int, costUSD float64, err error)

	ScheduledDeliveryStore
}

// ScheduledDeliveryStore linearizes a transcript mutation with the durable
// identity supplied by its external producer. Re-delivery of the same identity
// and fingerprint is a successful no-op; reuse with different semantics fails.
type ScheduledDeliveryStore interface {
	InsertToolNotificationPairOnce(
		ctx context.Context,
		sessionID int64,
		deliveryID, fingerprint string,
		assistant, toolResult *StoredMessage,
	) (asstID, resultID int64, inserted bool, err error)
	ResetSessionContextOnce(
		ctx context.Context,
		sessionID int64,
		deliveryID, fingerprint string,
		opening []*StoredMessage,
	) (messageIDs []int64, inserted bool, err error)
}

// OrchestrationStore is the capability used by the daemon. It owns session-row
// lifecycle and the two cross-table subagent transactions. It may read a child
// transcript and atomically append a delivered completion, but cannot perform
// ordinary loop-owned transcript rewrites or checkpoint loop-local state.
type OrchestrationStore interface { //nolint:interfacebloat // one bounded orchestration capability, kept at the 15-method cap
	CreateSession(
		ctx context.Context,
		projectID int64,
		model, reasoningLevel string,
		attrs map[string]any,
	) (*SessionRecord, error)
	CreateSubagentSession(
		ctx context.Context,
		projectID, parentID, rootID int64,
		agentType, model, reasoningLevel string,
	) (int64, error)
	SetAttributes(ctx context.Context, id int64, attrs map[string]any) error
	UpdateSessionModel(ctx context.Context, id int64, model, reasoningLevel string) error
	GetSession(ctx context.Context, id int64) (*SessionRecord, error)
	ListSessions(ctx context.Context) ([]*SessionRecord, error)
	ListAllSessions(ctx context.Context) ([]*SessionRecord, error)
	FindSessionByProjectID(ctx context.Context, projectID int64) (*SessionRecord, error)
	LatestActivityByProject(ctx context.Context, projectIDs []int64) (map[int64]time.Time, error)
	MarkSessionKilled(ctx context.Context, id int64) error
	UpdateSessionStatus(ctx context.Context, id int64, status SessionStatus) error
	KillTerminatingSessions(ctx context.Context) error
	LoadActiveMessages(ctx context.Context, sessionID int64) ([]*StoredMessage, error)
}

// Store is the complete persistence surface returned by NewStore. Consumers
// should accept RuntimeStore or OrchestrationStore unless they truly need both.
type Store interface { //nolint:interfacebloat // Complete constructor result; consumers use narrow capabilities.
	RuntimeStore
	BudgetResponseStore
	BudgetCompactionStore
	OrchestrationStore
	InboxStore
	OutputStore
	ManagerRootStore
	CommandOutputStore
	LifecycleOutputStore
	LifecycleCommandStore
	ReplacementStore
	ActivationStore
	DirectOutputStore
	BudgetStore
	ModelInputStore
	ProgressStore
	ReadinessStore
	StopCompletionStore
}

var (
	_ Store                 = (*store)(nil)
	_ RuntimeStore          = (*store)(nil)
	_ OrchestrationStore    = (*store)(nil)
	_ InboxStore            = (*store)(nil)
	_ OutputStore           = (*store)(nil)
	_ ManagerRootStore      = (*store)(nil)
	_ CommandOutputStore    = (*store)(nil)
	_ LifecycleOutputStore  = (*store)(nil)
	_ LifecycleCommandStore = (*store)(nil)
	_ ReplacementStore      = (*store)(nil)
	_ ActivationStore       = (*store)(nil)
	_ DirectOutputStore     = (*store)(nil)
	_ BudgetStore           = (*store)(nil)
	_ ModelInputStore       = (*store)(nil)
	_ ProgressStore         = (*store)(nil)
	_ ReadinessStore        = (*store)(nil)
	_ StopCompletionStore   = (*store)(nil)
)

type store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) CreateSession(
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

func (s *store) CreateSubagentSession(
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
		`INSERT INTO sessions (project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID,
		parentID,
		rootID,
		agentType,
		model,
		reasoningLevel,
		now,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert subagent session: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	return id, nil
}

func (s *store) SetAttributes(ctx context.Context, id int64, attrs map[string]any) error {
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

func (s *store) UpdateSessionModel(ctx context.Context, id int64, model, reasoningLevel string) error {
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

func (s *store) GetSession(ctx context.Context, id int64) (*SessionRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id,
	)

	return scanSession(row)
}

func (s *store) ListSessions(ctx context.Context) ([]*SessionRecord, error) {
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
func (s *store) ListAllSessions(ctx context.Context) ([]*SessionRecord, error) {
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

func (s *store) FindSessionByProjectID(ctx context.Context, projectID int64) (*SessionRecord, error) {
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
func (s *store) LatestActivityByProject(ctx context.Context, projectIDs []int64) (map[int64]time.Time, error) {
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

func (s *store) MarkSessionKilled(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func (s *store) UpdateSessionStatus(ctx context.Context, id int64, status SessionStatus) error {
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
func (s *store) KillTerminatingSessions(ctx context.Context) error {
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
func (s *store) GetChildSessionStats(ctx context.Context, rootID int64) (count, totalIterations int, err error) {
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
func (s *store) GetSessionTreeUsage(
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

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertMessageWith(ctx context.Context, q execer, sessionID int64, msg *StoredMessage) (int64, error) {
	result, err := q.ExecContext(
		ctx,
		`INSERT INTO messages (session_id, role, content, tool_call_id, tool_name, tool_calls, reasoning_content, reasoning_raw, attachments, cost_usd, usage) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		msg.Role,
		msg.Content,
		nullString(msg.ToolCallID),
		nullString(msg.ToolName),
		nullRawJSON(msg.ToolCalls),
		msg.ReasoningContent,
		nullRawJSON(msg.ReasoningRaw),
		nullRawJSON(msg.Attachments),
		msg.CostUSD,
		nullRawJSON(msg.Usage),
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

func (s *store) InsertMessage(ctx context.Context, sessionID int64, msg *StoredMessage) (int64, error) {
	return insertMessageWith(ctx, s.db, sessionID, msg)
}

func (s *store) MarkCompacted(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET compacted_at = ? WHERE id = ?`, now, id); err != nil {
			return fmt.Errorf("mark compacted %d: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func (s *store) ReplaceCompactedMessages(
	ctx context.Context,
	sessionID int64,
	compactedIDs []int64,
	entries []CompactionEntry,
) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin compaction replacement: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	ids, err := replaceCompactedMessagesTx(ctx, tx, sessionID, compactedIDs, entries, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit compaction replacement: %w", err)
	}

	return ids, nil
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
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET compacted_at = ? WHERE id = ?`, now, id); err != nil {
			return nil, fmt.Errorf("mark compacted %d: %w", id, err)
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

func (s *store) LoadActiveMessages(ctx context.Context, sessionID int64) ([]*StoredMessage, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, session_id, role, content, tool_call_id, tool_name, tool_calls, reasoning_content, reasoning_raw, attachments, cost_usd, usage, compacted_at, created_at
		FROM messages WHERE session_id = ? AND compacted_at IS NULL ORDER BY position IS NULL, position, id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query active messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

func (s *store) UpdateSessionIteration(
	ctx context.Context,
	id int64,
	iteration int,
	status SessionStatus,
) error {
	if !status.valid() {
		return fmt.Errorf("invalid session status %q", status)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`UPDATE sessions SET iteration = ?, status = ?, updated_at = ?
		WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('stopping', 'terminating', 'killed')`,
		iteration, status, now, id,
	)
	if err != nil {
		return fmt.Errorf("update session iteration: %w", err)
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

func (s *store) UpdateSessionTodoItems(ctx context.Context, id int64, items json.RawMessage) error {
	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`UPDATE sessions SET todo_items = ?, updated_at = ? WHERE id = ?`,
		string(items), now, id,
	)
	if err != nil {
		return fmt.Errorf("update session todo items: %w", err)
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

// killTerminatingTarget commits one root's killed transition and its close
// output in a single transaction: a crash after a committed killed UPDATE but
// before the close INSERT would strand the obligation on a root that is never
// re-selected.
func (s *store) killTerminatingTarget(ctx context.Context, id int64, owner string, now time.Time) error {
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
		if _, err := insertClosedOutput(ctx, tx, id, owner, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit terminating kill: %w", err)
	}

	return nil
}

func (s *store) latestActivity(ctx context.Context, projectID int64) (time.Time, bool, error) {
	t, ok, err := s.maxUpdatedAt(ctx, projectID, true)
	if err != nil || ok {
		return t, ok, err
	}

	return s.maxUpdatedAt(ctx, projectID, false)
}

func (s *store) maxUpdatedAt(ctx context.Context, projectID int64, excludeKilled bool) (time.Time, bool, error) {
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

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
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

	err := sc.Scan(&rec.ID, &projectID, &model, &reasoning, &masterEnabled, &attrsRaw,
		&agentType, &parentID, &iteration, &status, &todoItems,
		&rec.CreatedAt, &rec.UpdatedAt, &killedAt, &rootID,
		&rec.ModelInputGeneration, &boundary,
		&rec.ContextBaselineModel, &rec.ContextBaselinePromptTokens, &rec.ContextBaselineMessageCount)
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

	return &rec, nil
}

func scanMessages(rows *sql.Rows) ([]*StoredMessage, error) {
	var messages []*StoredMessage

	for rows.Next() {
		var msg StoredMessage

		var toolCallID, toolName, toolCallsRaw, reasoningContent, reasoningRaw, attachmentsRaw, usageRaw sql.NullString
		var compactedAt sql.NullTime
		var costUSD sql.NullFloat64

		err := rows.Scan(
			&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
			&toolCallID, &toolName, &toolCallsRaw, &reasoningContent, &reasoningRaw,
			&attachmentsRaw,
			&costUSD, &usageRaw, &compactedAt, &msg.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}

		msg.ToolCallID = toolCallID.String
		msg.ToolName = toolName.String

		if toolCallsRaw.Valid && toolCallsRaw.String != "" {
			msg.ToolCalls = json.RawMessage(toolCallsRaw.String)
		}

		msg.ReasoningContent = reasoningContent.String

		if reasoningRaw.Valid && reasoningRaw.String != "" {
			msg.ReasoningRaw = json.RawMessage(reasoningRaw.String)
		}

		if attachmentsRaw.Valid && attachmentsRaw.String != "" {
			msg.Attachments = json.RawMessage(attachmentsRaw.String)
		}

		msg.CostUSD = costUSD.Float64

		if usageRaw.Valid && usageRaw.String != "" {
			msg.Usage = json.RawMessage(usageRaw.String)
		}

		if compactedAt.Valid {
			msg.CompactedAt = &compactedAt.Time
		}

		messages = append(messages, &msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
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

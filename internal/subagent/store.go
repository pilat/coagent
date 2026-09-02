package subagent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const subagentLinkColumns = `parent_id, child_id, task_call_id, blocking, depth, state, delivered_at, delivered_msg_id, created_at, result, outcome, activation_seq`

// subagentLinkColumnsSL is subagentLinkColumns qualified with the "sl" alias,
// for queries that join subagent_links to sessions.
const subagentLinkColumnsSL = `sl.parent_id, sl.child_id, sl.task_call_id, sl.blocking, sl.depth, sl.state, sl.delivered_at, sl.delivered_msg_id, sl.created_at, sl.result, sl.outcome, sl.activation_seq`

var _ Store = (*store)(nil)

type store struct {
	db *sql.DB
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) InsertSubagentLink(ctx context.Context, link Link) error {
	if link.CreatedAt == 0 {
		link.CreatedAt = time.Now().UTC().Unix()
	}

	if link.State == "" {
		link.State = StateSpawned
	}

	if !link.State.valid() {
		return fmt.Errorf("insert subagent link: invalid state %q", link.State)
	}

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		link.ParentID,
		link.ChildID,
		link.TaskCallID,
		link.Blocking,
		link.Depth,
		link.State,
		link.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert subagent link: %w", err)
	}

	return nil
}

func (s *store) GetLink(ctx context.Context, childID int64) (*Link, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+subagentLinkColumns+` FROM subagent_links WHERE child_id = ? LIMIT 1`,
		childID,
	)

	return scanLinkRow(row)
}

func (s *store) GetLinkByTaskCallID(ctx context.Context, parentID int64, taskCallID string) (*Link, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+subagentLinkColumns+` FROM subagent_links WHERE parent_id = ? AND task_call_id = ? LIMIT 1`,
		parentID, taskCallID,
	)

	return scanLinkRow(row)
}

func (s *store) ListPendingChildLinks(ctx context.Context, parentID int64) ([]Link, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+subagentLinkColumns+` FROM subagent_links
		 WHERE parent_id = ? AND delivered_at IS NULL ORDER BY child_id`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending child links: %w", err)
	}
	defer rows.Close()

	return scanLinkRows(rows)
}

func (s *store) ListRunningChildLinks(ctx context.Context) ([]Link, error) {
	// The JOIN reads sessions.killed_at (read-only) to skip killed children — the
	// ledger observing session liveness, never writing it.
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+subagentLinkColumnsSL+` FROM subagent_links sl
		 JOIN sessions ch ON ch.id = sl.child_id
		 WHERE sl.state IN ('spawned', 'running') AND ch.killed_at IS NULL
		 ORDER BY sl.child_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query running child links: %w", err)
	}
	defer rows.Close()

	return scanLinkRows(rows)
}

func (s *store) ListUndeliveredParentLinks(ctx context.Context) ([]Link, error) {
	// The JOIN reads sessions.killed_at (read-only) to skip killed parents — the
	// ledger observing session liveness, never writing it.
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+subagentLinkColumnsSL+` FROM subagent_links sl
		 JOIN sessions p ON p.id = sl.parent_id
		 WHERE sl.state IN ('completed', 'error', 'killed') AND sl.delivered_at IS NULL AND p.killed_at IS NULL
		 ORDER BY sl.child_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query undelivered parent links: %w", err)
	}
	defer rows.Close()

	return scanLinkRows(rows)
}

// MarkLinkTerminal sets the link state, result, and outcome. result/outcome
// overwrite unconditionally (so a re-engaged child's second terminalization
// replaces the prior run's values). The caller must have committed the child's
// final message first (call-ordering guarantee, Appendix G7).
func (s *store) MarkLinkTerminal(
	ctx context.Context,
	childID int64,
	state State,
	result string,
	outcome Outcome,
) error {
	if !state.valid() || !outcome.valid() || !validTerminalLink(state, outcome) {
		return fmt.Errorf("invalid terminal link state/outcome %q/%q", state, outcome)
	}

	resultExec, err := s.db.ExecContext(
		ctx,
		`UPDATE subagent_links SET state = ?, result = ?, outcome = ? WHERE child_id = ?`,
		state, result, outcome, childID,
	)
	if err != nil {
		return fmt.Errorf("update link state: %w", err)
	}

	rows, err := resultExec.RowsAffected()
	if err != nil {
		return fmt.Errorf("link rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("subagent link for child %d not found", childID)
	}

	return nil
}

// ResetLinkRunning re-arms a link for a follow-up message: state back to running,
// delivery marker cleared so a new completion is owed. result/outcome are left
// stale until the next MarkLinkTerminal overwrites them; childSnapshot guards
// reads with the terminal check so the stale value is never surfaced mid-rerun.
func (s *store) ResetLinkRunning(ctx context.Context, childID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE subagent_links
		 SET state = ?, blocking = 0, activation_seq = activation_seq + 1,
		     delivered_at = NULL, delivered_msg_id = NULL
		 WHERE child_id = ?`,
		StateRunning, childID,
	)
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

	return nil
}

func (s *store) MarkLinkStopped(ctx context.Context, childID int64) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE subagent_links SET state = ?
		WHERE child_id = ? AND state IN ('spawned', 'running')`, StateStopped, childID)
	if err != nil {
		return fmt.Errorf("mark link stopped: %w", err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("stopped link rows affected: %w", err)
	}

	return nil
}

// MakeStoppedLinkResumable detaches a stopped foreground child from the task
// call that Stop resolves in its parent. A later explicit follow-up then runs it
// as a background continuation and reports through the normal completion event.
func (s *store) MakeStoppedLinkResumable(ctx context.Context, childID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE subagent_links SET blocking = 0
		WHERE child_id = ? AND state = 'stopped'`, childID)
	if err != nil {
		return fmt.Errorf("make stopped link resumable: %w", err)
	}

	return nil
}

func scanLinkRow(row *sql.Row) (*Link, error) {
	link, err := scanLinkFrom(row)
	if errors.Is(err, sql.ErrNoRows) {
		//nolint:nilnil // established "not found" contract of Store.Get*, relied on by every caller in this package
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return link, nil
}

func scanLinkRows(rows *sql.Rows) ([]Link, error) {
	var links []Link

	for rows.Next() {
		link, err := scanLinkFrom(rows)
		if err != nil {
			return nil, err
		}

		links = append(links, *link)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subagent links: %w", err)
	}

	return links, nil
}

func scanLinkFrom(sc rowScanner) (*Link, error) {
	var link Link
	var deliveredAt, deliveredMsgID sql.NullInt64
	var state, outcome string

	err := sc.Scan(
		&link.ParentID, &link.ChildID, &link.TaskCallID, &link.Blocking, &link.Depth,
		&state, &deliveredAt, &deliveredMsgID, &link.CreatedAt,
		&link.Result, &outcome, &link.ActivationSeq,
	)
	if err != nil {
		// %w keeps sql.ErrNoRows matchable via errors.Is for scanLinkRow's caller.
		return nil, fmt.Errorf("scan subagent link: %w", err)
	}

	link.DeliveredAt = deliveredAt.Int64
	link.DeliveredMsgID = deliveredMsgID.Int64
	link.State = State(state)

	link.Outcome = Outcome(outcome)
	if !link.State.valid() {
		return nil, fmt.Errorf("invalid persisted link state %q for child %d", link.State, link.ChildID)
	}

	if link.Outcome != "" && !link.Outcome.valid() {
		return nil, fmt.Errorf("invalid persisted link outcome %q for child %d", link.Outcome, link.ChildID)
	}

	return &link, nil
}

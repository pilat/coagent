package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	WatchKindAnswer    = "answer"
	WatchKindStep      = "step"
	WatchKindSent      = "sent"
	WatchKindScheduled = "scheduled"
	WatchKindNotice    = "notice"
	WatchKindClosed    = "closed"
)

type WatchRead struct {
	WatcherID         int64
	Chain             []int64
	Latest            bool
	MaxRows, MaxRunes int
}

type WatchPage struct {
	Name                                 string
	Rows                                 []WatchRow
	Skipped, Remaining, RemainingAnswers int
}

type WatchRow struct {
	ID        int64
	Kind      string
	Content   string
	Clipped   int
	CreatedAt time.Time
}

// ReadWatch selects a bounded page and advances only this watcher's position atomically.
func (s *Store) ReadWatch(ctx context.Context, read WatchRead) (*WatchPage, error) {
	if len(read.Chain) == 0 || read.MaxRows <= 0 || read.MaxRunes <= 0 {
		return nil, errors.New("watch read requires a chain and positive page bounds")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin watch read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	page := &WatchPage{}
	if err := readWatchTx(ctx, tx, read, page); err != nil {
		return nil, fmt.Errorf("read session watch: %w", err)
	}

	page.Name, err = chatOutputName(ctx, tx, read.Chain[len(read.Chain)-1])
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit watch read: %w", err)
	}

	return page, nil
}

func readWatchTx(ctx context.Context, tx *sql.Tx, read WatchRead, page *WatchPage) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(read.Chain)), ",")

	args := make([]any, 1, 1+len(read.Chain))

	args[0] = read.WatcherID
	for _, id := range read.Chain {
		args = append(args, id)
	}

	var position sql.NullInt64
	var positions int

	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT MAX(read_through), COUNT(*) FROM session_watches
		WHERE watcher_id = ? AND target_id IN (%s)`, placeholders), args...).Scan(&position, &positions)
	if err != nil {
		return fmt.Errorf("load watch position: %w", err)
	}

	read.Latest = read.Latest || positions == 0

	outputArgs := append(slices.Clone(args[1:]), position.Int64)
	var total, answers int

	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(source_key GLOB 'message:*:final'), 0)
		FROM session_outbox WHERE session_id IN (%s) AND id > ?
		AND type NOT IN ('session_opened','session_replaced')`, placeholders), outputArgs...).Scan(&total, &answers)
	if err != nil {
		return fmt.Errorf("count unread outputs: %w", err)
	}

	page.Rows, err = watchRows(ctx, tx, read, placeholders, outputArgs)
	if err != nil {
		return err
	}

	if positions == 0 && len(page.Rows) == 0 {
		return nil
	}

	cursor := position.Int64
	for _, row := range page.Rows {
		cursor = max(cursor, row.ID)
		if row.Kind == WatchKindAnswer {
			answers--
		}
	}

	if read.Latest {
		page.Skipped = total - len(page.Rows)
	} else {
		page.Remaining = total - len(page.Rows)
		page.RemainingAnswers = answers
	}

	return saveWatchPosition(ctx, tx, read, placeholders, args, cursor)
}

func watchRows(ctx context.Context, tx *sql.Tx, read WatchRead, placeholders string, args []any) ([]WatchRow, error) {
	query := `SELECT id, type, content, source_key, created_at FROM session_outbox
		WHERE session_id IN (%s) AND id > ? AND type NOT IN ('session_opened','session_replaced')
		ORDER BY id ASC LIMIT ?`
	if read.Latest {
		query = `SELECT id, type, content, source_key, created_at FROM session_outbox
			WHERE session_id IN (%s) AND id > ? AND type NOT IN ('session_opened','session_replaced')
			ORDER BY id DESC LIMIT ?`
	}

	rows, err := tx.QueryContext(ctx, fmt.Sprintf(query, placeholders), append(args, read.MaxRows)...)
	if err != nil {
		return nil, fmt.Errorf("load watch outputs: %w", err)
	}
	defer rows.Close()

	var result []WatchRow

	remaining := read.MaxRunes

	for rows.Next() {
		row, err := scanWatchRow(rows)
		if err != nil {
			return nil, err
		}

		size := utf8.RuneCountInString(row.Content)
		if size > remaining && len(result) > 0 {
			break
		}

		if size > remaining {
			row.Content = string([]rune(row.Content)[:remaining])
			row.Clipped = size - remaining
		}

		result = append(result, row)
		remaining -= min(size, remaining)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watch outputs: %w", err)
	}

	if read.Latest {
		slices.Reverse(result)
	}

	return result, nil
}

func scanWatchRow(scanner rowScanner) (WatchRow, error) {
	var row WatchRow
	var kind OutputType
	var key sql.NullString

	if err := scanner.Scan(&row.ID, &kind, &row.Content, &key, &row.CreatedAt); err != nil {
		return WatchRow{}, fmt.Errorf("scan watch output: %w", err)
	}

	row.Kind = watchKind(kind, key.String)

	return row, nil
}

func saveWatchPosition(
	ctx context.Context, tx *sql.Tx, read WatchRead, placeholders string, args []any, cursor int64,
) error {
	newest := read.Chain[len(read.Chain)-1]

	_, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM session_watches
		WHERE watcher_id = ? AND target_id IN (%s) AND target_id <> ?`, placeholders), append(args, newest)...)
	if err != nil {
		return fmt.Errorf("rekey watch position: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO session_watches (watcher_id, target_id, read_through)
		VALUES (?, ?, ?) ON CONFLICT(watcher_id, target_id) DO UPDATE
		SET read_through = MAX(session_watches.read_through, excluded.read_through)`, read.WatcherID, newest, cursor)
	if err != nil {
		return fmt.Errorf("save watch position: %w", err)
	}

	return nil
}

func watchKind(kind OutputType, key string) string {
	switch {
	case kind == OutputSessionClosed:
		return WatchKindClosed
	case strings.HasPrefix(key, "message:") && strings.HasSuffix(key, ":"+OutputPhaseFinal):
		return WatchKindAnswer
	case strings.HasPrefix(key, "message:") && strings.HasSuffix(key, ":"+OutputPhaseReply):
		return WatchKindStep
	case strings.HasPrefix(key, "input:") && strings.HasSuffix(key, ":sent"):
		return WatchKindSent
	case strings.HasPrefix(key, "schedule:") && strings.HasSuffix(key, ":announcement"):
		return WatchKindScheduled
	default:
		return WatchKindNotice
	}
}

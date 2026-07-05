package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	scheduleColumns = `id, session_id, cron_expr, one_shot_at, input_message, last_fired_at, metadata, fire_count, fresh, created_at`
)

type (
	ScheduleMetadata struct {
		ToolCallID string `json:"tool_call_id,omitempty"`
	}

	Schedule struct {
		id           int64
		sessionID    int64
		cronExpr     string
		oneShotAt    *time.Time
		inputMessage string
		lastFiredAt  *time.Time
		metadata     ScheduleMetadata
		fireCount    int
		fresh        bool
		createdAt    time.Time
	}

	Store interface {
		AddSchedule(
			ctx context.Context,
			sessionID int64,
			cronExpr string,
			oneShotAt *time.Time,
			inputMessage string,
			fresh bool,
		) (*Schedule, error)
		AddScheduleWithMeta(
			ctx context.Context,
			sessionID int64,
			cronExpr string,
			oneShotAt *time.Time,
			inputMessage string,
			fresh bool,
			meta ScheduleMetadata,
		) (*Schedule, error)
		RemoveSchedule(ctx context.Context, id int64) error
		RemoveScheduleForSession(ctx context.Context, sessionID int64, scheduleID int64) error
		RemoveOneShotSchedules(ctx context.Context, sessionID int64) (int64, error)
		RemoveSleepSchedules(ctx context.Context, sessionID int64) (int64, error)
		RemoveAllSchedules(ctx context.Context, sessionID int64) error
		ListSchedules(ctx context.Context, sessionID int64) ([]*Schedule, error)
		ListDueSchedules(ctx context.Context, now time.Time) ([]*Schedule, error)
		ListDueCronSchedules(ctx context.Context, now time.Time) ([]*Schedule, error)
		UpdateScheduleLastFired(ctx context.Context, id int64, t time.Time) error
	}

	store struct {
		db *sql.DB
	}
)

var _ Store = (*store)(nil)

func (s *Schedule) ID() int64               { return s.id }
func (s *Schedule) SessionID() int64        { return s.sessionID }
func (s *Schedule) CronExpr() string        { return s.cronExpr }
func (s *Schedule) OneShotAt() *time.Time   { return s.oneShotAt }
func (s *Schedule) InputMessage() string    { return s.inputMessage }
func (s *Schedule) LastFiredAt() *time.Time { return s.lastFiredAt }
func (s *Schedule) Fresh() bool             { return s.fresh }

func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) AddSchedule(
	ctx context.Context,
	sessionID int64,
	cronExpr string,
	oneShotAt *time.Time,
	inputMessage string,
	fresh bool,
) (*Schedule, error) {
	return s.AddScheduleWithMeta(ctx, sessionID, cronExpr, oneShotAt, inputMessage, fresh, ScheduleMetadata{})
}

func (s *store) AddScheduleWithMeta(
	ctx context.Context,
	sessionID int64,
	cronExpr string,
	oneShotAt *time.Time,
	inputMessage string,
	fresh bool,
	meta ScheduleMetadata,
) (*Schedule, error) {
	if cronExpr == "" && oneShotAt == nil {
		return nil, errors.New("schedule must have cron_expr or one_shot_at")
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO schedules (session_id, cron_expr, one_shot_at, input_message, metadata, fresh, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		nullString(cronExpr),
		nullTime(oneShotAt),
		inputMessage,
		string(metaJSON),
		fresh,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert schedule: %w", err)
	}

	newID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}

	sched := &Schedule{
		id:           newID,
		sessionID:    sessionID,
		cronExpr:     cronExpr,
		oneShotAt:    oneShotAt,
		inputMessage: inputMessage,
		metadata:     meta,
		fresh:        fresh,
		createdAt:    now,
	}

	return sched, nil
}

func (s *store) RemoveSchedule(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("schedule %d not found", id)
	}

	return nil
}

func (s *store) RemoveScheduleForSession(ctx context.Context, sessionID, scheduleID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM schedules WHERE id = ? AND session_id = ?`,
		scheduleID,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("schedule %d not found", scheduleID)
	}

	return nil
}

func (s *store) RemoveOneShotSchedules(ctx context.Context, sessionID int64) (int64, error) {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM schedules WHERE session_id = ? AND one_shot_at IS NOT NULL`,
		sessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete one-shot schedules: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return rows, nil
}

// RemoveSleepSchedules deletes only one-shot rows that own an exact suspended
// sleep tool call. Generic one-shot schedules are standalone future inputs and
// must survive interruption of the current sleep.
func (s *store) RemoveSleepSchedules(ctx context.Context, sessionID int64) (int64, error) {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM schedules
		 WHERE session_id = ?
		   AND one_shot_at IS NOT NULL
		   AND COALESCE(json_extract(metadata, '$.tool_call_id'), '') <> ''`,
		sessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete sleep schedules: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sleep schedule rows affected: %w", err)
	}

	return rows, nil
}

// RemoveAllSchedules deletes every schedule (one-shot and cron) for a session —
// used on session kill, which owns the full teardown regardless of schedule kind.
func (s *store) RemoveAllSchedules(ctx context.Context, sessionID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete schedules: %w", err)
	}

	return nil
}

func (s *store) ListSchedules(ctx context.Context, sessionID int64) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE session_id = ? ORDER BY created_at`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query schedules: %w", err)
	}
	defer rows.Close()

	return scanSchedules(rows)
}

func (s *store) ListDueSchedules(ctx context.Context, now time.Time) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE one_shot_at IS NOT NULL AND one_shot_at <= ?`,
		now.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("query due schedules: %w", err)
	}
	defer rows.Close()

	return scanSchedules(rows)
}

func (s *store) ListDueCronSchedules(ctx context.Context, now time.Time) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE cron_expr IS NOT NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("query cron schedules: %w", err)
	}
	defer rows.Close()

	return scanSchedules(rows)
}

func (s *store) UpdateScheduleLastFired(ctx context.Context, id int64, t time.Time) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE schedules SET last_fired_at = ?, fire_count = fire_count + 1 WHERE id = ?`,
		t.UTC(),
		id,
	)
	if err != nil {
		return fmt.Errorf("update last_fired_at: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("schedule %d not found", id)
	}

	return nil
}

func scanSchedules(rows *sql.Rows) ([]*Schedule, error) {
	var schedules []*Schedule

	for rows.Next() {
		var sched Schedule

		var cronExpr, inputMsg, metaRaw sql.NullString
		var oneShotAt, lastFired sql.NullTime
		var fireCount sql.NullInt64

		err := rows.Scan(
			&sched.id,
			&sched.sessionID,
			&cronExpr,
			&oneShotAt,
			&inputMsg,
			&lastFired,
			&metaRaw,
			&fireCount,
			&sched.fresh,
			&sched.createdAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}

		sched.cronExpr = cronExpr.String

		if oneShotAt.Valid {
			sched.oneShotAt = &oneShotAt.Time
		}

		sched.inputMessage = inputMsg.String

		if lastFired.Valid {
			sched.lastFiredAt = &lastFired.Time
		}

		if metaRaw.Valid && metaRaw.String != "" {
			if err := json.Unmarshal([]byte(metaRaw.String), &sched.metadata); err != nil {
				return nil, fmt.Errorf("unmarshal schedule metadata: %w", err)
			}
		}

		sched.fireCount = int(fireCount.Int64)
		schedules = append(schedules, &sched)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedule rows: %w", err)
	}

	return schedules, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}

	return sql.NullTime{Time: *t, Valid: true}
}

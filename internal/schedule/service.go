package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

type Service interface {
	ListSchedules(ctx context.Context, sessionID int64) ([]Entry, error)
	PendingSleeps(ctx context.Context, sessionID int64) ([]PendingSleep, error)
	RemoveSchedule(ctx context.Context, sessionID int64, scheduleID int64) error
	// AddSleep records a one-shot wake that resolves one exact suspended tool
	// call. A sleep without callID cannot be resumed honestly and is rejected.
	AddSleep(
		ctx context.Context,
		sessionID int64,
		callID string,
		wakeAt time.Time,
		result string,
	) (Created, error)
	AddRecurring(
		ctx context.Context,
		sessionID int64,
		cronExpr string,
		inputMessage string,
		fresh bool,
	) (Created, error)
	CancelPendingSleeps(ctx context.Context, sessionID int64) (int64, error)
	RemoveAllForSession(ctx context.Context, sessionID int64) error
}

var _ Service = (*svc)(nil)

type svc struct {
	store Store
}

func NewService(store Store) Service {
	return &svc{store: store}
}

func (s *svc) ListSchedules(ctx context.Context, sessionID int64) ([]Entry, error) {
	schedules, err := s.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}

	result := make([]Entry, len(schedules))

	for i, sc := range schedules {
		result[i] = sc
	}

	return result, nil
}

func (s *svc) PendingSleeps(ctx context.Context, sessionID int64) ([]PendingSleep, error) {
	schedules, err := s.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list pending sleeps: %w", err)
	}

	var pending []PendingSleep

	for _, sched := range schedules {
		if sched.oneShotAt != nil && sched.metadata.ToolCallID != "" {
			pending = append(pending, PendingSleep{
				CallID: sched.metadata.ToolCallID,
				WakeAt: *sched.oneShotAt,
			})
		}
	}

	return pending, nil
}

func (s *svc) RemoveSchedule(ctx context.Context, sessionID, scheduleID int64) error {
	if err := s.store.RemoveScheduleForSession(ctx, sessionID, scheduleID); err != nil {
		return fmt.Errorf("remove schedule: %w", err)
	}

	return nil
}

func (s *svc) CancelPendingSleeps(ctx context.Context, sessionID int64) (int64, error) {
	n, err := s.store.RemoveSleepSchedules(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("cancel pending sleeps: %w", err)
	}

	return n, nil
}

func (s *svc) RemoveAllForSession(ctx context.Context, sessionID int64) error {
	if err := s.store.RemoveAllSchedules(ctx, sessionID); err != nil {
		return fmt.Errorf("remove all schedules: %w", err)
	}

	return nil
}

func (s *svc) AddRecurring(
	ctx context.Context,
	sessionID int64,
	cronExpr string,
	inputMessage string,
	fresh bool,
) (Created, error) {
	if cronExpr == "" {
		return Created{}, errors.New("add recurring schedule: cron expression is required")
	}

	sched, err := s.store.AddSchedule(ctx, sessionID, cronExpr, nil, inputMessage, fresh)
	if err != nil {
		return Created{}, fmt.Errorf("add recurring schedule: %w", err)
	}

	return describeRecurring(sched, cronExpr), nil
}

func (s *svc) AddSleep(
	ctx context.Context,
	sessionID int64,
	callID string,
	wakeAt time.Time,
	result string,
) (Created, error) {
	if callID == "" {
		return Created{}, errors.New("add sleep: tool call id is required")
	}

	sched, err := s.store.AddScheduleWithMeta(
		ctx,
		sessionID,
		"",
		&wakeAt,
		result,
		false,
		ScheduleMetadata{ToolCallID: callID},
	)
	if err != nil {
		return Created{}, fmt.Errorf("add sleep: %w", err)
	}

	return describeSleep(sched, wakeAt), nil
}

func describeRecurring(sched *Schedule, cronExpr string) Created {
	created := Created{ID: sched.id, Type: "cron"}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	if schedule, err := parser.Parse(cronExpr); err == nil {
		created.NextFire = schedule.Next(time.Now()).UTC()
	}

	return created
}

func describeSleep(sched *Schedule, wakeAt time.Time) Created {
	return Created{ID: sched.id, Type: "interval", NextFire: wakeAt.UTC()}
}

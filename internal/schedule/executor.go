package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/tool"
)

// maxOneShotAttempts bounds delivery retries for a one-shot schedule. An
// undeliverable wake (e.g. its session was removed) is dropped after this many
// ticks instead of retrying every 10s forever.
const maxOneShotAttempts = 10

type SessionSender interface {
	DeliverPendingCallResult(
		ctx context.Context, sessionID int64, callID, toolName, content string,
	) (bool, error)
	DeliverScheduleTick(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
	DeliverFreshSchedule(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
	NotifySession(sessionID int64, n sessionevent.Notification)
}

type Executor interface {
	Start(ctx context.Context)
	Stop()
}

var _ Executor = (*executor)(nil)

type executor struct {
	store  Store
	sender SessionSender
	cancel context.CancelFunc
	done   chan struct{}

	// oneShotAttempts counts consecutive delivery failures per one-shot schedule
	// id. Mutated only from the single tick goroutine, so it needs no lock.
	oneShotAttempts map[int64]int
}

func NewExecutor(store Store, sender SessionSender) Executor {
	return &executor{
		store:           store,
		sender:          sender,
		done:            make(chan struct{}),
		oneShotAttempts: make(map[int64]int),
	}
}

func (e *executor) Start(ctx context.Context) {
	ctx, e.cancel = context.WithCancel(ctx)
	go e.loop(ctx)
}

func (e *executor) Stop() {
	if e.cancel != nil {
		e.cancel()
	}

	<-e.done
}

func (e *executor) loop(ctx context.Context) {
	defer close(e.done)

	l := logger.Ctx(ctx).Named("schedule.executor")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	e.tick(ctx, l)

	for {
		select {
		case <-ticker.C:
			e.tick(ctx, l)
		case <-ctx.Done():
			return
		}
	}
}

func (e *executor) tick(ctx context.Context, l *zap.Logger) {
	now := time.Now()

	if err := e.fireOneShotSchedules(ctx, now, l); err != nil {
		l.Warn("one_shot_error", zap.Error(err))
	}

	if err := e.fireCronSchedules(ctx, now, l); err != nil {
		l.Warn("cron_error", zap.Error(err))
	}
}

func (e *executor) fireOneShotSchedules(ctx context.Context, now time.Time, l *zap.Logger) error {
	schedules, err := e.store.ListDueSchedules(ctx, now)
	if err != nil {
		return fmt.Errorf("list due schedules: %w", err)
	}

	for _, sched := range schedules {
		l.Info("firing_one_shot", zap.Int64("schedule_id", sched.id), zap.Int64("session_id", sched.sessionID))

		applied, err := e.deliverOneShot(ctx, sched)
		if err != nil {
			e.handleOneShotFailure(ctx, sched, err, l)
			continue
		}

		delete(e.oneShotAttempts, sched.id)

		if applied {
			e.sender.NotifySession(sched.sessionID, sessionevent.Notification{
				Type:    sessionevent.NotifyInputReceived,
				Message: sched.inputMessage,
				Source:  "scheduler",
			})
		}

		if err := e.store.RemoveSchedule(ctx, sched.id); err != nil {
			l.Warn("remove_schedule_failed", zap.Int64("schedule_id", sched.id), zap.Error(err))
		}
	}

	return nil
}

func (e *executor) deliverOneShot(ctx context.Context, sched *Schedule) (bool, error) {
	if sched.metadata.ToolCallID != "" {
		applied, err := e.sender.DeliverPendingCallResult(
			ctx,
			sched.sessionID,
			sched.metadata.ToolCallID,
			tool.IDSleep,
			sched.inputMessage,
		)
		if err != nil {
			return false, fmt.Errorf("deliver sleep result for schedule %d: %w", sched.id, err)
		}

		return applied, nil
	}

	if sched.fresh {
		applied, err := e.sender.DeliverFreshSchedule(
			ctx, sched.sessionID, oneShotDeliveryID(sched.id), sched.inputMessage,
		)
		if err != nil {
			return false, fmt.Errorf("deliver fresh schedule %d: %w", sched.id, err)
		}

		return applied, nil
	}

	applied, err := e.sender.DeliverScheduleTick(
		ctx, sched.sessionID, oneShotDeliveryID(sched.id), sched.inputMessage,
	)
	if err != nil {
		return false, fmt.Errorf("deliver schedule tick %d: %w", sched.id, err)
	}

	return applied, nil
}

// handleOneShotFailure records a delivery failure and drops the schedule once
// retries are exhausted, so an undeliverable wake cannot retry every tick forever.
func (e *executor) handleOneShotFailure(ctx context.Context, sched *Schedule, sendErr error, l *zap.Logger) {
	e.oneShotAttempts[sched.id]++
	attempts := e.oneShotAttempts[sched.id]

	l.Warn(
		"send_failed",
		zap.Int64("schedule_id", sched.id),
		zap.Int64("session_id", sched.sessionID),
		zap.Int("attempts", attempts),
		zap.Error(sendErr),
	)

	if attempts < maxOneShotAttempts {
		return
	}

	l.Warn("one_shot_undeliverable", zap.Int64("schedule_id", sched.id), zap.Int("attempts", attempts))

	if err := e.store.RemoveSchedule(ctx, sched.id); err != nil {
		l.Warn("remove_schedule_failed", zap.Int64("schedule_id", sched.id), zap.Error(err))
		return
	}

	delete(e.oneShotAttempts, sched.id)
}

func (e *executor) fireCronSchedules(ctx context.Context, now time.Time, l *zap.Logger) error {
	schedules, err := e.store.ListDueCronSchedules(ctx, now)
	if err != nil {
		return fmt.Errorf("list cron schedules: %w", err)
	}

	truncatedNow := now.Truncate(time.Minute)
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	for _, sched := range schedules {
		cronExpr, loc := parseCronTZ(sched.cronExpr)

		schedule, err := parser.Parse(cronExpr)
		if err != nil {
			l.Warn(
				"invalid_cron",
				zap.Int64("schedule_id", sched.id),
				zap.String("expr", sched.cronExpr),
				zap.Error(err),
			)

			continue
		}

		localNow := truncatedNow.In(loc)
		nextTime := schedule.Next(localNow.Add(-time.Minute))

		if !nextTime.Equal(localNow) {
			continue
		}

		if sched.lastFiredAt != nil && sched.lastFiredAt.Truncate(time.Minute).Equal(truncatedNow) {
			continue
		}

		l.Info(
			"firing_cron",
			zap.Int64("schedule_id", sched.id),
			zap.Int64("session_id", sched.sessionID),
			zap.String("expr", sched.cronExpr),
		)

		// A fresh schedule delivers only its prompt and asks the session to wipe
		// its context first; a normal one appends a tick header to live history.
		content := sched.inputMessage
		if !sched.fresh {
			fireCount := sched.fireCount + 1
			content = fmt.Sprintf("Schedule tick #%d (cron: %s). Current time: %s",
				fireCount, sched.cronExpr, truncatedNow.UTC().Format(time.RFC3339))

			if sched.inputMessage != "" {
				content += "\n\nTask: " + sched.inputMessage
			}
		}

		deliveryID := cronDeliveryID(sched.id, truncatedNow)

		applied, sendErr := e.deliverCronSchedule(ctx, sched, deliveryID, content)
		if sendErr != nil {
			l.Warn(
				"send_failed",
				zap.Int64("schedule_id", sched.id),
				zap.Int64("session_id", sched.sessionID),
				zap.Error(sendErr),
			)

			continue
		}

		// A cron occurrence is acknowledged only after the session accepted it.
		// Otherwise the next tick retries instead of losing the event behind a
		// pending external tool call or a transient transcript write failure.
		if err := e.store.UpdateScheduleLastFired(ctx, sched.id, now); err != nil {
			l.Warn("update_last_fired_failed", zap.Int64("schedule_id", sched.id), zap.Error(err))
		}

		if applied {
			e.sender.NotifySession(sched.sessionID, sessionevent.Notification{
				Type:    sessionevent.NotifyInputReceived,
				Message: content,
				Source:  "scheduler",
			})
		}
	}

	return nil
}

func (e *executor) deliverCronSchedule(
	ctx context.Context,
	sched *Schedule,
	deliveryID, content string,
) (bool, error) {
	if sched.fresh {
		applied, err := e.sender.DeliverFreshSchedule(ctx, sched.sessionID, deliveryID, content)
		if err != nil {
			return false, fmt.Errorf("deliver fresh schedule: %w", err)
		}

		return applied, nil
	}

	applied, err := e.sender.DeliverScheduleTick(ctx, sched.sessionID, deliveryID, content)
	if err != nil {
		return false, fmt.Errorf("deliver schedule tick: %w", err)
	}

	return applied, nil
}

func oneShotDeliveryID(scheduleID int64) string {
	return fmt.Sprintf("schedule:one-shot:%d", scheduleID)
}

func cronDeliveryID(scheduleID int64, occurrence time.Time) string {
	return fmt.Sprintf(
		"schedule:cron:%d:%s",
		scheduleID,
		occurrence.UTC().Format("20060102T1504Z"),
	)
}

// SplitCronTZ splits a stored cron expression into its 5-field part and timezone
// name (in that order). A bare expression (no CRON_TZ= prefix) reports "UTC".
func SplitCronTZ(expr string) (string, string) {
	const prefix = "CRON_TZ="
	if !strings.HasPrefix(expr, prefix) {
		return expr, "UTC"
	}

	tz, cronPart, ok := strings.Cut(expr[len(prefix):], " ")
	if !ok {
		return expr, "UTC"
	}

	return cronPart, tz
}

func parseCronTZ(expr string) (string, *time.Location) {
	cronPart, tzName := SplitCronTZ(expr)

	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return cronPart, time.UTC
	}

	return cronPart, loc
}

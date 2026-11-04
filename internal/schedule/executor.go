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
)

// maxOneShotAttempts bounds delivery retries for a one-shot schedule. An
// undeliverable wake (e.g. its session was removed) is dropped after this many
// ticks instead of retrying every 10s forever.
const maxOneShotAttempts = 10

type SessionSender interface {
	NotifySession(sessionID int64, n sessionevent.Notification)
}

// Executor delivers due schedules until stopped.
type Executor struct {
	store  Store
	sender SessionSender
	cancel context.CancelFunc
	done   chan struct{}

	// oneShotAttempts counts consecutive delivery failures per one-shot schedule
	// id. Mutated only from the single tick goroutine, so it needs no lock.
	oneShotAttempts map[int64]int
}

func NewExecutor(store Store, sender SessionSender) *Executor {
	return &Executor{
		store:           store,
		sender:          sender,
		done:            make(chan struct{}),
		oneShotAttempts: make(map[int64]int),
	}
}

func (e *Executor) Start(ctx context.Context) {
	ctx, e.cancel = context.WithCancel(ctx)
	go e.loop(ctx)
}

func (e *Executor) Stop() {
	if e.cancel != nil {
		e.cancel()
	}

	<-e.done
}

func (e *Executor) loop(ctx context.Context) {
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

func (e *Executor) tick(ctx context.Context, l *zap.Logger) {
	now := time.Now()

	if err := e.fireOneShotSchedules(ctx, now, l); err != nil {
		l.Warn("one_shot_error", zap.Error(err))
	}

	if err := e.fireCronSchedules(ctx, now, l); err != nil {
		l.Warn("cron_error", zap.Error(err))
	}
}

func (e *Executor) fireOneShotSchedules(ctx context.Context, now time.Time, l *zap.Logger) error {
	schedules, err := e.store.ListDueSchedules(ctx, now)
	if err != nil {
		return fmt.Errorf("list due schedules: %w", err)
	}

	for _, sched := range schedules {
		l.Info("firing_one_shot", zap.Int64("schedule_id", sched.id), zap.Int64("session_id", sched.sessionID))

		var applied bool
		if sched.metadata.ToolCallID != "" {
			applied, err = e.store.DeliverSleep(ctx, sched)
		} else {
			applied, err = e.store.DeliverOneShot(ctx, sched, sched.inputMessage, scheduleAttributes(sched))
		}

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
	}

	return nil
}

// handleOneShotFailure records a delivery failure and drops the schedule once
// retries are exhausted, so an undeliverable wake cannot retry every tick forever.
func (e *Executor) handleOneShotFailure(ctx context.Context, sched *Schedule, sendErr error, l *zap.Logger) {
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

func (e *Executor) fireCronSchedules(ctx context.Context, now time.Time, l *zap.Logger) error {
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

		applied, sendErr := e.store.DeliverCron(ctx, sched, content, scheduleAttributes(sched), truncatedNow, now)
		if sendErr != nil {
			l.Warn(
				"send_failed",
				zap.Int64("schedule_id", sched.id),
				zap.Int64("session_id", sched.sessionID),
				zap.Error(sendErr),
			)

			continue
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

func scheduleAttributes(sched *Schedule) map[string]any {
	return map[string]any{"fresh": sched.fresh}
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

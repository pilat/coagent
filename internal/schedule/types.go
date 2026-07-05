package schedule

import "time"

// Entry is a stored schedule as read by the schedule tool.
type Entry interface {
	ID() int64
	CronExpr() string
	OneShotAt() *time.Time
	InputMessage() string
	LastFiredAt() *time.Time
	Fresh() bool
}

// Created describes a freshly added schedule.
type Created struct {
	ID       int64
	Type     string // "interval" or "cron"
	NextFire time.Time
}

// PendingSleep is the domain projection of a one-shot row that owns an exact
// suspended sleep tool call. Generic one-shot schedules are future inputs and
// intentionally do not appear in this view.
type PendingSleep struct {
	CallID string
	WakeAt time.Time
}

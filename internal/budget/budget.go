//nolint:wrapcheck // Service preserves store sentinels used for CAS arbitration.; nosemgrep: semgrep.coagent-no-preamble-before-package
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Service interface {
	Get(ctx context.Context, rootID int64) (*Record, error)
	Observe(
		ctx context.Context,
		rootID int64,
		persistedCost float64,
		observedAt time.Time,
		assistantText string,
	) (*Record, bool, error)
	Admit(ctx context.Context, rootID int64, now time.Time) error
	BeginDrain(ctx context.Context, rootID, generation int64, owner string) (*Record, error)
	MarkParked(ctx context.Context, rootID, generation int64, owner string) (*Record, error)
	Release(ctx context.Context, rootID, generation int64, reason string) (*Record, error)
	ListPendingParks(ctx context.Context) ([]*Record, error)
	ListArmed(ctx context.Context) ([]*Record, error)
}

type Grant struct {
	RootID     int64
	InputID    int64
	ToolID     string
	Command    string
	ToolCallID string
}

type svc struct {
	store PolicyStore
}

var _ Service = (*svc)(nil)

func New(store PolicyStore) Service {
	return &svc{store: store}
}

func (s *svc) BeginDrain(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*Record, error) {
	record, err := s.store.BeginBudgetDrain(ctx, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("begin budget drain: %w", err)
	}

	return record, nil
}

func (s *svc) MarkParked(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*Record, error) {
	record, err := s.store.MarkBudgetParked(ctx, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("mark budget parked: %w", err)
	}

	return record, nil
}

func (s *svc) Release(
	ctx context.Context,
	rootID, generation int64,
	reason string,
) (*Record, error) {
	return s.store.ReleaseBudget(ctx, rootID, generation, reason)
}

func (s *svc) ListPendingParks(ctx context.Context) ([]*Record, error) {
	return s.store.ListPendingBudgetParks(ctx)
}

func (s *svc) ListArmed(ctx context.Context) ([]*Record, error) {
	return s.store.ListArmedBudgets(ctx)
}

func (s *svc) Get(ctx context.Context, rootID int64) (*Record, error) {
	return s.store.Get(ctx, rootID)
}

// Observe delegates to the store's single-transaction observation: the
// crossing decision and the fire share one writer serialization, so the
// recorded delta can never lag behind the persisted cost it compares against.
func (s *svc) Observe(
	ctx context.Context,
	rootID int64,
	_ float64,
	observedAt time.Time,
	assistantText string,
) (*Record, bool, error) {
	return s.store.ObserveBudget(ctx, rootID, observedAt, assistantText)
}

func (s *svc) Admit(ctx context.Context, rootID int64, now time.Time) error {
	record, err := s.store.Get(ctx, rootID)
	if errors.Is(err, ErrNotFound) || (err == nil && record.State == Released) {
		return nil
	}

	if err != nil {
		return err
	}

	if record.State == Fired {
		return errors.New("budget checkpoint fired")
	}

	if now.Before(record.ArmedAt) {
		return errors.New("current clock predates the durable budget baseline")
	}

	return nil
}

func renderLimits(cost *float64, duration *time.Duration) string {
	parts := ""
	if cost != nil {
		parts = fmt.Sprintf("$%.6f additional persisted cost", *cost)
	}

	if duration != nil {
		if parts != "" {
			parts += " or "
		}

		parts += duration.String() + " wall time"
	}

	return parts
}

//nolint:wrapcheck // Service preserves store sentinels used for CAS arbitration.; nosemgrep: semgrep.coagent-no-preamble-before-package
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

type Service interface {
	Get(ctx context.Context, rootID int64) (*sessionstore.BudgetRecord, error)
	Set(
		ctx context.Context,
		grant Grant,
		cost *float64,
		duration *time.Duration,
	) (*sessionstore.BudgetRecord, string, error)
	Clear(ctx context.Context, grant Grant) (*sessionstore.BudgetRecord, string, error)
	Observe(
		ctx context.Context,
		rootID int64,
		persistedCost float64,
		observedAt time.Time,
		assistantText string,
	) (*sessionstore.BudgetRecord, bool, error)
	Admit(ctx context.Context, rootID int64, now time.Time) error
	BeginDrain(ctx context.Context, rootID, generation int64, owner string) (*sessionstore.BudgetRecord, error)
	MarkParked(ctx context.Context, rootID, generation int64, owner string) (*sessionstore.BudgetRecord, error)
	Release(ctx context.Context, rootID, generation int64, reason string) (*sessionstore.BudgetRecord, error)
	ListPendingParks(ctx context.Context) ([]*sessionstore.BudgetRecord, error)
	ListArmed(ctx context.Context) ([]*sessionstore.BudgetRecord, error)
}

type Grant struct {
	RootID     int64
	InputID    int64
	ToolID     string
	Command    string
	ToolCallID string
}

type svc struct {
	store sessionstore.BudgetStore
}

var _ Service = (*svc)(nil)

func New(store sessionstore.BudgetStore) Service {
	return &svc{store: store}
}

func (s *svc) BeginDrain(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*sessionstore.BudgetRecord, error) {
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
) (*sessionstore.BudgetRecord, error) {
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
) (*sessionstore.BudgetRecord, error) {
	return s.store.ReleaseBudget(ctx, rootID, generation, reason)
}

func (s *svc) ListPendingParks(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
	return s.store.ListPendingBudgetParks(ctx)
}

func (s *svc) ListArmed(ctx context.Context) ([]*sessionstore.BudgetRecord, error) {
	return s.store.ListArmedBudgets(ctx)
}

func (s *svc) Get(ctx context.Context, rootID int64) (*sessionstore.BudgetRecord, error) {
	return s.store.GetBudget(ctx, rootID)
}

func (s *svc) Set(
	ctx context.Context,
	grant Grant,
	cost *float64,
	duration *time.Duration,
) (*sessionstore.BudgetRecord, string, error) {
	if cost == nil && duration == nil {
		return nil, "", errors.New("set requires cost_usd, duration, or both")
	}
	var seconds *int64

	if duration != nil {
		value := int64(duration.Seconds())
		seconds = &value
	}

	receipt := "Budget armed: " + renderLimits(cost, duration)
	record, _, err := s.store.ArmBudget(ctx, sessionstore.BudgetMutation{
		RootSessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID,
		Command: grant.Command, ToolCallID: grant.ToolCallID, CostLimitUSD: cost,
		DurationSeconds: seconds, Receipt: receipt,
	})

	return record, receipt, err
}

func (s *svc) Clear(
	ctx context.Context,
	grant Grant,
) (*sessionstore.BudgetRecord, string, error) {
	const receipt = "Budget cleared"
	record, _, err := s.store.ClearBudget(ctx, sessionstore.BudgetMutation{
		RootSessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID,
		Command: grant.Command, ToolCallID: grant.ToolCallID, Receipt: receipt,
	})

	return record, receipt, err
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
) (*sessionstore.BudgetRecord, bool, error) {
	return s.store.ObserveBudget(ctx, rootID, observedAt, assistantText)
}

func (s *svc) Admit(ctx context.Context, rootID int64, now time.Time) error {
	record, err := s.store.GetBudget(ctx, rootID)
	if errors.Is(err, sessionstore.ErrBudgetNotFound) || (err == nil && record.State == sessionstore.BudgetReleased) {
		return nil
	}

	if err != nil {
		return err
	}

	if record.State == sessionstore.BudgetFired {
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

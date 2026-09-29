package sessionlifecycle

import (
	"context"
	"fmt"
)

// StopEffects binds producer cleanup to the shared runner stop protocol.
type StopEffects struct {
	CancelProcesses func(context.Context, int64) error
	RetireResources func(context.Context, int64) error
	// The stopping fence excludes new inputs before this phase scans pending commands.
	SettleControlInputs func(context.Context, []int64) error
	SettleCalls         func(context.Context, int64) error
	ExpireActivation    func(context.Context, int64) error
	CancelSleeps        func(context.Context, int64) error
}

// StopTree requires the caller's tree fence and preserves it through durable settlement.
func (s *supervisor[T]) StopTree(
	ctx context.Context,
	rootID int64,
	stopper Stopper,
	keepRootStopping bool,
	effects StopEffects,
) error {
	ctx = context.WithoutCancel(ctx)

	records, err := s.sessions.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list live tree runners: %w", err)
	}
	var liveIDs []int64

	for _, record := range records {
		if record.ID != rootID && record.RootID != rootID {
			continue
		}

		if _, ok := s.runners.Load(record.ID); ok {
			liveIDs = append(liveIDs, record.ID)
		}
	}

	plan, err := stopper.Begin(ctx, rootID, liveIDs)
	if err != nil {
		return fmt.Errorf("begin stop tree: %w", err)
	}

	ids := plan.SessionIDs()
	s.RemoveQueued(ids)

	runners := make([]Runner[T], 0, len(ids))
	for _, id := range ids {
		if r, ok := s.runners.Load(id); ok {
			runners = append(runners, r)
		}
	}
	// Foreground parents may await children; signal the entire tree before joining.
	for _, r := range runners {
		r.Cancel()
	}

	if err := effects.CancelProcesses(ctx, rootID); err != nil {
		return err
	}

	for _, r := range runners {
		<-r.Done()
	}

	if err := effects.RetireResources(ctx, rootID); err != nil {
		return err
	}

	return settleStoppedTree(ctx, stopper, plan, ids, keepRootStopping, effects)
}

func settleStoppedTree(
	ctx context.Context,
	stopper Stopper,
	plan *StopPlan,
	ids []int64,
	keepRootStopping bool,
	effects StopEffects,
) error {
	if effects.SettleControlInputs != nil {
		if err := effects.SettleControlInputs(ctx, ids); err != nil {
			return fmt.Errorf("settle stopped control inputs: %w", err)
		}
	}

	if err := stopper.CancelInputs(ctx, plan); err != nil {
		return fmt.Errorf("cancel stopped inputs: %w", err)
	}

	for _, id := range ids {
		if err := effects.SettleCalls(ctx, id); err != nil {
			return err
		}

		if err := effects.ExpireActivation(ctx, id); err != nil {
			return err
		}

		if err := effects.CancelSleeps(ctx, id); err != nil {
			return err
		}
	}

	if err := stopper.Finish(ctx, plan, keepRootStopping); err != nil {
		return fmt.Errorf("finish stop tree: %w", err)
	}

	return nil
}

package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// Apply requires persisted suspension; a rejected commit delivers its verdict without restarting.
func (s *externalCalls) Apply(ctx context.Context, sessionID int64, shuttingDown func() bool) {
	callID, sc, ok := s.staged.takePendingApply(sessionID)
	if !ok {
		return
	}

	// An unbacked marker strands its verdict and turns a later boot failure into a rollback.
	backed, err := s.suspendIsDurable(ctx, sessionID, callID, sc.toolName)
	if err != nil || !backed {
		s.releaseUnbackedApply(ctx, sessionID, callID, sc, err, shuttingDown)

		return
	}

	log := logger.Ctx(ctx).Named("daemon.apply")

	v := s.applier.Apply(sc.apply, configops.Pending{
		SessionID:  sessionID,
		ToolCallID: callID,
		ToolName:   sc.toolName,
	})

	if !v.Failed() {
		log.Info("apply_committed", zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName))

		// Spend the grant before another input can advance. A crash here is settled
		// from the marker by the next boot.
		s.ConsumeActivation(ctx, sessionID, callID)

		return
	}

	log.Warn("apply_rejected", zap.Int64("session_id", sessionID), zap.String("reason", v.Reason()))

	// The ledger entry stays until the rejection is durably injected — releasing
	// it here leaves the session's own result with no producer that owns it.
	if err := s.enqueue(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: sc.toolName},
		Content: "Config change rejected — " + v.Reason(),
	}); err != nil {
		log.Error("rejection_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// ExpireActivation follows stop settlement; consumed grants cannot expire.
func (s *externalCalls) ExpireActivation(ctx context.Context, sessionID int64) error {
	activation, err := s.activationStore.PendingActivation(ctx, sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load pending activation for session %d: %w", sessionID, err)
	}

	if _, err := s.activationStore.ExpireActivation(ctx, activation.InputID, sessionID); err != nil {
		return fmt.Errorf("expire activation for session %d: %w", sessionID, err)
	}

	return nil
}

// ConsumeActivation is idempotent so live apply and marker recovery share the same settlement.
func (s *externalCalls) ConsumeActivation(ctx context.Context, sessionID int64, callID string) {
	activation, err := s.activationStore.CurrentActivation(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, sessionstore.ErrActivationNotFound) {
			logger.Ctx(ctx).Named("daemon.apply").Warn(
				"read_config_edit_activation", zap.Int64("session_id", sessionID), zap.Error(err))
		}

		return
	}

	if activation.ToolID != tool.IDConfigEdit || activation.Command != configtools.ConfigEditCommand {
		return
	}

	binding := sessionstore.ActivationBinding{
		InputID: activation.InputID, SessionID: sessionID,
		ToolID: activation.ToolID, Command: activation.Command, ToolCallID: callID,
	}
	if err := s.activationStore.ConsumeActivationBinding(ctx, binding); err != nil {
		logger.Ctx(ctx).Named("daemon.apply").Warn(
			"consume_config_edit_activation", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// Abandon releases the global slot while retaining the verdict.
// Transcript settlement waits for the runner's lifecycle fence.
func (s *externalCalls) Abandon(ctx context.Context, sessionID int64) {
	if s.applier == nil {
		return
	}

	callID, sc, ok := s.staged.takePendingApply(sessionID)
	if !ok {
		return
	}

	s.applier.ReleaseApply()
	s.staged.stageResult(sessionID, callID, sc.toolName,
		"Config change abandoned — the session ended before it was applied. Nothing was written.")

	log := logger.Ctx(ctx).Named("daemon.apply")
	log.Warn("apply_abandoned", zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName))
}

// SettleResults requires the caller's transcript-writer fence after abandonment.
func (s *externalCalls) SettleResults(ctx context.Context, sessionID int64) error {
	for callID, sc := range s.staged.pendingResults(sessionID) {
		durable, err := s.suspendIsDurable(ctx, sessionID, callID, sc.toolName)
		if err != nil {
			return err
		}

		// Expire first: a crash after the result must not leave an unbound grant
		// blocking the FIFO with no unresolved call for recovery to settle.
		if err := s.expireAbandonedActivation(ctx, sessionID, callID, sc.toolName); err != nil {
			return err
		}

		if durable {
			if err := s.settleAbandonedApply(ctx, sessionID, pendingCallResultInput{
				Call: session.PendingToolCall{ID: callID, Name: sc.toolName}, Content: sc.result,
			}); err != nil {
				return err
			}
		}

		s.staged.resolve(sessionID, callID)
	}

	return nil
}

// suspendIsDurable reports whether the durable transcript carries callID as an
// unresolved external call of toolName — the precondition for committing.
func (s *externalCalls) suspendIsDurable(ctx context.Context, sessionID int64, callID, toolName string) (bool, error) {
	calls, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("read suspended call %s of session %d: %w", callID, sessionID, err)
	}

	return pendingCall(calls, callID, toolName), nil
}

// releaseUnbackedApply gives the slot back for a staged change whose suspend the
// transcript does not back. Nothing is written, so the change is simply dropped.
func (s *externalCalls) releaseUnbackedApply(
	ctx context.Context,
	sessionID int64,
	callID string,
	sc stagedCall,
	scanErr error,
	shuttingDown func() bool,
) {
	s.applier.ReleaseApply()

	log := logger.Ctx(ctx).Named("daemon.apply")
	log.Warn("apply_suspend_not_durable",
		zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName), zap.Error(scanErr))

	// A call the transcript demonstrably lacks has nobody waiting; only an
	// unverifiable read is worth answering, since an owner-less call bricks a session.
	if scanErr == nil || shuttingDown() {
		s.staged.resolve(sessionID, callID)

		return
	}

	if err := s.enqueue(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: sc.toolName},
		Content: "Config change abandoned — the suspend could not be verified, so nothing was written.",
	}); err != nil {
		log.Error("unbacked_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

func (s *externalCalls) settleAbandonedApply(ctx context.Context, sessionID int64, input pendingCallResultInput) error {
	sess, err := s.openTranscript(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("open transcript for abandoned apply: %w", err)
	}

	_, err = sess.ResolvePendingCall(ctx, input.Call, input.Content)
	if err != nil {
		return fmt.Errorf("settle abandoned apply: %w", err)
	}

	return nil
}

func (s *externalCalls) expireAbandonedActivation(ctx context.Context, sessionID int64, callID, toolName string) error {
	activation, err := s.activationStore.PendingActivation(ctx, sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read abandoned activation: %w", err)
	}

	if activation.ToolID != toolName || (activation.ToolCallID != "" && activation.ToolCallID != callID) {
		return nil
	}

	_, err = s.activationStore.ExpireActivation(ctx, activation.InputID, sessionID)
	if err != nil {
		return fmt.Errorf("expire abandoned activation: %w", err)
	}

	return nil
}

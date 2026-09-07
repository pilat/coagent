package backgroundprocess

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (s *svc) supervise(ctx context.Context, launched *launchResult) {
	release := sync.OnceFunc(func() {
		s.releaseTracked(launched.record.ID, launched.record.SessionID)
		launched.cancel()
	})
	defer release()

	natural, exitCode := s.waitForExit(ctx, launched)
	if err := launched.collector.Close(); err != nil {
		natural = StateOutputDrainTimeout
		exitCode = nil
	}

	if natural == StateOutputDrainTimeout {
		exitCode = nil
	}

	if launched.collector.overflowed() {
		natural = StateOutputLimitExceeded
		exitCode = nil
	}

	finalized, won, err := s.finalizeTracked(ctx, launched, natural, exitCode)
	if err != nil {
		logger.Ctx(ctx).Named("backgroundprocess.supervise").Warn(
			"process_finalize_failed", zap.String("process", launched.record.ID), zap.Error(err),
		)

		return
	}

	if won {
		launched.record = finalized
	}

	release()
	s.emit(ctx, launched.record, won)
}

func (s *svc) waitForExit(ctx context.Context, launched *launchResult) (State, *int) {
	timer := time.NewTimer(time.Until(launched.record.Deadline))
	defer timer.Stop()

	flush := time.NewTicker(outputSizeFlush)
	defer flush.Stop()

	waitErr := make(chan error, 1)
	go func() { waitErr <- launched.cmd.Wait() }()

	for {
		select {
		case err := <-waitErr:
			state, exitCode := classifyExit(err)
			if errors.Is(err, exec.ErrWaitDelay) {
				_ = killGroup(launched.cmd)
			}

			return state, exitCode
		case <-timer.C:
			if _, err := s.store.RecordIntent(ctx, launched.record.ID, IntentDeadline); err != nil {
				logger.Ctx(ctx).Named("backgroundprocess.supervise").Warn(
					"deadline_intent_failed", zap.String("process", launched.record.ID), zap.Error(err),
				)
			}

			_ = killGroup(launched.cmd)

			<-waitErr

			return StateTimedOut, nil
		case <-flush.C:
			if err := s.store.UpdateOutputSize(ctx, launched.record.ID, launched.collector.Size()); err != nil {
				logger.Ctx(ctx).Named("backgroundprocess.supervise").Warn(
					"output_size_update_failed", zap.String("process", launched.record.ID), zap.Error(err),
				)
			}
		}
	}
}

func classifyExit(err error) (State, *int) {
	if errors.Is(err, exec.ErrWaitDelay) {
		return StateOutputDrainTimeout, nil
	}

	code := 0
	if err == nil {
		return StateCompleted, &code
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		failed := exitErr.ExitCode()

		return StateFailed, &failed
	}

	return StateOutputDrainTimeout, nil
}

func (s *svc) emit(ctx context.Context, record Process, won bool) {
	if s.opts.OnCompletion == nil || !won {
		return
	}

	if record.AdvertisedAt == nil || record.WakeSuppressed() || record.State == StateInterrupted {
		return
	}

	tail, truncated, ok := ExtractTail(record.OutputPath, TailPreviewLines, TailPreviewBytes)
	binaryTail := !ok || !ValidEventTail(tail)

	if binaryTail {
		tail = ""
	} else if truncated {
		tail = TruncateTailToBytes(tail, TailPreviewBytes)
	}

	completion := Completion{
		ProcessID: record.ID, SessionID: record.SessionID, RootID: record.RootSessionID,
		ToolCallID: record.ToolCallID, State: record.State,
		Duration: record.FinishedAt.Sub(record.CreatedAt), OutputPath: record.OutputPath,
		OutputSize: record.OutputSize, Tail: tail, TailOmitted: binaryTail, BinaryTail: binaryTail,
	}
	if record.ExitCode != nil {
		completion.ExitCode = *record.ExitCode
		completion.HasExitCode = true
	}

	s.opts.OnCompletion(ctx, completion)
}

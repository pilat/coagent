package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (s *svc) launch(
	ctx context.Context,
	spec Spec,
	spawn func(ctx context.Context) (*exec.Cmd, error),
) (*launchResult, error) {
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	cmd, err := spawn(processCtx)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("spawn background process: %w", err)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("cancel process group: %w", err)
		}

		return nil
	}
	cmd.WaitDelay = drainTimeout

	collector, record, quotaReady, err := s.prepareOutput(processCtx, spec, cmd)
	if err != nil {
		cancel()

		return nil, err
	}

	cmd.Stdout = collector
	cmd.Stderr = collector

	if err := cmd.Start(); err != nil {
		_ = collector.Close()

		cancel()

		return nil, fmt.Errorf("start background process: %w", err)
	}

	return &launchResult{
		cmd: cmd, collector: collector, record: record, quotaReady: quotaReady, cancel: cancel,
	}, nil
}

func (s *svc) prepareOutput(
	ctx context.Context,
	spec Spec,
	cmd *exec.Cmd,
) (*collector, Process, chan<- bool, error) {
	processID := s.newProcessID()
	outputDir := filepath.Join(s.opts.OutputDir, strconv.FormatInt(spec.SessionID, 10))

	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, Process{}, nil, fmt.Errorf("create process output dir: %w", err)
	}

	outputPath := filepath.Join(outputDir, processID+".output")

	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, Process{}, nil, fmt.Errorf("create process output file: %w", err)
	}

	now := s.opts.Now()

	record := Process{
		ID: processID, SessionID: spec.SessionID, RootSessionID: spec.RootSessionID,
		ToolCallID: spec.ToolCallID, OutputPath: outputPath, Deadline: now.Add(spec.Deadline),
		CreatedAt: now, State: StateRunning,
	}
	if spec.Advertise {
		record.AdvertisedAt = &now
	}

	quotaReady := make(chan bool, 1)
	collector := newCollector(file, MaxOutputBytes, func() {
		if persisted := <-quotaReady; persisted {
			if _, err := s.store.RecordIntent(ctx, processID, IntentOutputLimit); err != nil {
				logger.Ctx(ctx).Named("backgroundprocess.output").Warn(
					"output_limit_intent_failed", zap.String("process", processID), zap.Error(err),
				)
			}
		}

		_ = killGroup(cmd)
	})

	return collector, record, quotaReady, nil
}

func (s *svc) newProcessID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ids++

	return fmt.Sprintf("bgp_%d_%d", s.opts.Now().UnixNano(), s.ids)
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}

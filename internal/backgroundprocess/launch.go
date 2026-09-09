package backgroundprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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

	collector, record, quotaReady, err := s.prepareOutput(processCtx, spec, cmd)
	if err != nil {
		cancel()

		return nil, err
	}

	guardian, leaseWriter, err := s.startGuardian(processCtx, record.OutputPath)
	if err != nil {
		_ = collector.Close()

		cancel()

		return nil, err
	}

	processGroup := guardian.Process.Pid
	cmd.SysProcAttr = commandProcessSysProcAttr(processGroup)
	cmd.Cancel = func() error {
		if err := killProcessGroup(processGroup); err != nil {
			return fmt.Errorf("cancel process group: %w", err)
		}

		return nil
	}
	cmd.WaitDelay = drainTimeout

	cmd.Stdout = collector
	cmd.Stderr = collector

	if err := cmd.Start(); err != nil {
		_ = leaseWriter.Close()
		_ = guardian.Wait()
		_ = collector.Close()

		cancel()

		return nil, fmt.Errorf("start background process: %w", err)
	}

	return &launchResult{
		cmd: cmd, guardian: guardian, leaseWriter: leaseWriter, processGroup: processGroup,
		collector: collector, record: record, quotaReady: quotaReady, cancel: cancel,
	}, nil
}

func (s *svc) startGuardian(ctx context.Context, outputPath string) (*exec.Cmd, *os.File, error) {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create guardian readiness pipe: %w", err)
	}
	defer readyReader.Close()

	leaseReader, leaseWriter, err := os.Pipe()
	if err != nil {
		_ = readyWriter.Close()

		return nil, nil, fmt.Errorf("create guardian lease pipe: %w", err)
	}

	guardian := s.opts.GuardianCommand(outputPath+".guard", readyWriter, leaseReader)

	guardian.SysProcAttr = guardianProcessSysProcAttr()
	if err := guardian.Start(); err != nil {
		_ = readyWriter.Close()
		_ = leaseReader.Close()
		_ = leaseWriter.Close()

		return nil, nil, fmt.Errorf("start process guardian: %w", err)
	}

	_ = readyWriter.Close()
	_ = leaseReader.Close()

	ready := make(chan error, 1)

	go func() {
		line, readErr := bufio.NewReader(readyReader).ReadString('\n')
		if readErr != nil {
			ready <- fmt.Errorf("read process guardian readiness: %w", readErr)

			return
		}

		line = strings.TrimSpace(line)
		if line != guardianReady {
			ready <- fmt.Errorf("process guardian readiness: %s", line)

			return
		}

		ready <- nil
	}()

	fail := func(err error) (*exec.Cmd, *os.File, error) {
		_ = leaseWriter.Close()
		_ = guardian.Process.Kill()
		_ = guardian.Wait()

		return nil, nil, err
	}

	select {
	case err := <-ready:
		if err != nil {
			return fail(err)
		}

		return guardian, leaseWriter, nil
	case <-ctx.Done():
		return fail(fmt.Errorf("arm process guardian: %w", ctx.Err()))
	case <-time.After(joinGrace):
		return fail(errors.New("arm process guardian: readiness timeout"))
	}
}

func (s *svc) prepareOutput(
	ctx context.Context,
	spec Spec,
	cmd *exec.Cmd,
) (*collector, Process, chan<- bool, error) {
	processID := s.newProcessID()

	if spec.ProjectDir == "" || filepath.Base(spec.ProjectDir) != spec.ProjectDir || spec.ProjectDir == "." {
		return nil, Process{}, nil, errors.New("invalid process project directory")
	}

	outputDir := filepath.Join(s.opts.OutputDir, spec.ProjectDir, strconv.FormatInt(spec.SessionID, 10))

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

	processGroup := cmd.Process.Pid
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Pgid > 0 {
		processGroup = cmd.SysProcAttr.Pgid
	}

	return killProcessGroup(processGroup)
}

func killProcessGroup(processGroup int) error {
	if processGroup <= 0 {
		return nil
	}

	if err := syscall.Kill(-processGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}

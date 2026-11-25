package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

const processCaptureLimit = 64 * 1024

type ProcessRun struct {
	Argv  []string
	Env   []string
	Stdin []byte
}

type ProcessOutput struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type processCapture struct {
	data []byte
}

func (s *Stack) WorkDir() string {
	return s.workDir
}

func (s *Stack) RunProcess(ctx context.Context, run ProcessRun) (ProcessOutput, error) {
	confined, _ := s.runner.(shellenv.ConfinedRunner)

	cmd, err := s.provider.WrapExec(ctx, confined, s.workDir, run.Argv, run.Env)
	if err != nil {
		return ProcessOutput{}, fmt.Errorf("prepare confined process: %w", err)
	}

	output, err := runProcess(cmd, run.Stdin)
	if ctx.Err() != nil {
		return output, ctx.Err()
	}

	return output, err
}

func (c *processCapture) Write(data []byte) (int, error) {
	remaining := processCaptureLimit - len(c.data)
	c.data = append(c.data, data[:min(remaining, len(data))]...)

	return len(data), nil
}

func runProcess(cmd *exec.Cmd, stdin []byte) (ProcessOutput, error) {
	defer procexec.CloseExtraFiles(cmd)

	configureCommandCancellation(cmd)

	stdout := &processCapture{}
	stderr := &processCapture{}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	output := ProcessOutput{
		Stdout:   stdout.data,
		Stderr:   stderr.data,
		ExitCode: -1,
	}

	if cmd.ProcessState != nil {
		output.ExitCode = cmd.ProcessState.ExitCode()
	}

	if output.ExitCode >= 0 {
		return output, nil
	}

	return output, fmt.Errorf("run confined process: %w", err)
}

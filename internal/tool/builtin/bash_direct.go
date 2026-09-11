package builtin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/tool"
)

type inlineWriter struct {
	mu        sync.Mutex
	output    []byte
	truncated bool
}

func (t *bashTool) runDirect(
	ctx context.Context,
	p bashParams,
	workDir string,
	deadline time.Duration,
) (*tool.Result, error) {
	if p.Background {
		return nil, errors.New("background mode is unavailable: no process service")
	}

	processCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	cmd, err := t.runner.ShellCommand(processCtx, p.Command, workDir)
	if err != nil {
		return nil, fmt.Errorf("create bash command: %w", err)
	}

	output := &inlineWriter{}
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()

	code := 0

	state := backgroundprocess.StateCompleted
	if runErr != nil {
		state = backgroundprocess.StateFailed

		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	if errors.Is(processCtx.Err(), context.DeadlineExceeded) {
		state = backgroundprocess.StateTimedOut
		code = -1
	}

	text, truncated := output.result()
	if state == backgroundprocess.StateTimedOut {
		text += "\n\n(Command timed out after " + deadline.String() + ")"
	}

	if state == backgroundprocess.StateFailed {
		if hint := sandboxHint(text, t.runner.WritableRoots(), t.runner.ReadScope(), t.workDir); hint != "" {
			text += "\n\n" + hint
		}
	}

	if truncated {
		text += "\n\n(Output truncated)"
	}

	return &tool.Result{
		Title:   bashTitle(p.Command),
		Output:  text,
		IsError: state != backgroundprocess.StateCompleted,
		Metadata: map[string]any{
			metaKeyExitCode: code, metaKeyTimedOut: state == backgroundprocess.StateTimedOut,
			metaKeyTruncated: truncated,
		},
	}, nil
}

func (w *inlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := maxOutputSize - len(w.output)
	if remaining > 0 {
		w.output = append(w.output, p[:min(len(p), remaining)]...)
	}

	if len(p) > remaining {
		w.truncated = true
	}

	return len(p), nil
}

func (w *inlineWriter) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if strings.TrimSpace(string(w.output)) == "" {
		return noOutput, w.truncated
	}

	return strings.TrimSuffix(string(w.output), "\n"), w.truncated
}

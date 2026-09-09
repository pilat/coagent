package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/tool"
)

func (t *bashTool) waitForegroundTerminal(
	ctx context.Context,
	record backgroundprocess.Process,
	command string,
) (*tool.Result, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Until(record.Deadline.Add(foregroundGrace)))
	defer timer.Stop()

	lifecycleCtx := context.WithoutCancel(ctx)
	var lastErr error

	for {
		current, err := t.process.Store().GetProcess(lifecycleCtx, record.ID)
		if err != nil {
			lastErr = err
		} else if current.State.Terminal() {
			return t.foregroundResult(ctx, current, command), nil
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			cause := errors.New("foreground process did not terminalize after its deadline")
			if lastErr != nil {
				cause = fmt.Errorf("load foreground process: %w", lastErr)
			}

			return nil, t.abortCandidate(lifecycleCtx, record, cause)
		}
	}
}

// waitForeground polls the ledger until the grace expires; foreground
// completion is produced here rather than through a wake event.
func (t *bashTool) waitForeground(
	ctx context.Context,
	record backgroundprocess.Process,
	until time.Time,
	command string,
) (*tool.Result, bool) {
	ledgerCtx := context.WithoutCancel(ctx)

	for time.Now().Before(until) {
		current, err := t.process.Store().GetProcess(ledgerCtx, record.ID)
		if err != nil {
			time.Sleep(25 * time.Millisecond)

			continue
		}

		if current.State.Terminal() {
			return t.foregroundResult(ctx, current, command), true
		}

		time.Sleep(25 * time.Millisecond)
	}

	current, err := t.process.Store().GetProcess(ledgerCtx, record.ID)
	if err == nil && current.State.Terminal() {
		return t.foregroundResult(ctx, current, command), true
	}

	return nil, false
}

func (t *bashTool) foregroundResult(
	ctx context.Context,
	record backgroundprocess.Process,
	command string,
) *tool.Result {
	title := bashTitle(command)

	//nolint:exhaustive // Only terminal states reach this switch.
	switch record.State {
	case backgroundprocess.StateCompleted:
		output, truncated := t.inlineOutput(ctx, record)

		return &tool.Result{
			Title:  title,
			Output: output,
			Metadata: map[string]any{
				metaKeyProcessID: record.ID,
				metaKeyExitCode:  *record.ExitCode,
				metaKeyTimedOut:  false,
				metaKeyTruncated: truncated,
			},
		}
	case backgroundprocess.StateFailed:
		output, truncated := t.inlineOutput(ctx, record)
		if hint := t.failureHint(output); hint != "" {
			output += "\n\n" + hint
		}

		return &tool.Result{
			Title:   title,
			Output:  output,
			IsError: true,
			Metadata: map[string]any{
				metaKeyExitCode:  *record.ExitCode,
				metaKeyTimedOut:  false,
				metaKeyTruncated: truncated,
			},
		}
	case backgroundprocess.StateTimedOut:
		output, truncated := t.inlineOutput(ctx, record)
		output += "\n\n(Command timed out after " + record.Deadline.Sub(record.CreatedAt).String() + ")"

		return &tool.Result{
			Title:   title,
			Output:  output,
			IsError: true,
			Metadata: map[string]any{
				metaKeyExitCode:  -1,
				metaKeyTimedOut:  true,
				metaKeyTruncated: truncated,
			},
		}
	case backgroundprocess.StateOutputLimitExceeded:
		return &tool.Result{
			Title: title,
			Output: "Output exceeded the 100 MiB capture cap; the process group was killed." +
				"\nFull output: " + record.OutputPath,
			IsError: true,
			Metadata: map[string]any{
				metaKeyProcessID: record.ID,
			},
		}
	case backgroundprocess.StateCancelled, backgroundprocess.StateInterrupted:
		output, truncated := t.retainedInlineOutput(record)

		return terminatedForegroundResult(title, record, output, truncated)
	default:
		output, truncated := t.inlineOutput(ctx, record)

		return terminatedForegroundResult(title, record, output, truncated)
	}
}

func terminatedForegroundResult(
	title string,
	record backgroundprocess.Process,
	output string,
	truncated bool,
) *tool.Result {
	return &tool.Result{
		Title:   title,
		Output:  output + "\n\n(command terminated: " + string(record.State) + ")",
		IsError: true,
		Metadata: map[string]any{
			metaKeyExitCode:  -1,
			metaKeyTimedOut:  false,
			metaKeyTruncated: truncated,
		},
	}
}

func (t *bashTool) retainedInlineOutput(record backgroundprocess.Process) (string, bool) {
	text, truncated, ok := backgroundprocess.ExtractTail(record.OutputPath, 100000, maxOutputSize)
	if !ok {
		return "(text preview unavailable)\nOutput file: " + record.OutputPath, true
	}

	if strings.TrimSpace(text) == "" {
		text = noOutput
	} else {
		text = strings.TrimSuffix(text, "\n")
	}

	if truncated {
		text = "(output truncated at the inline limit)"
	}

	return text + "\nOutput file: " + record.OutputPath, truncated
}

func (t *bashTool) inlineOutput(ctx context.Context, record backgroundprocess.Process) (string, bool) {
	text, truncated, ok := backgroundprocess.ExtractTail(record.OutputPath, 100000, maxOutputSize)
	if !ok {
		return "(text preview unavailable)\nOutput file: " + record.OutputPath, true
	}

	if strings.TrimSpace(text) == "" {
		return t.consumeInlineCandidate(ctx, record, noOutput)
	}

	if truncated {
		return "(output truncated at the inline limit)\n" + t.spillNotice(record), true
	}

	return t.consumeInlineCandidate(ctx, record, strings.TrimSuffix(text, "\n"))
}

func (t *bashTool) consumeInlineCandidate(
	ctx context.Context,
	record backgroundprocess.Process,
	text string,
) (string, bool) {
	if err := t.removeCandidate(ctx, record); err != nil {
		return text + retainedOutputNotice(record, err), false
	}

	return text, false
}

func (t *bashTool) removeCandidate(ctx context.Context, record backgroundprocess.Process) error {
	if t.process != nil {
		if err := t.process.RemoveOutput(context.WithoutCancel(ctx), record.ID); err != nil {
			return fmt.Errorf("remove inline candidate: %w", err)
		}
	}

	return nil
}

func retainedOutputNotice(record backgroundprocess.Process, err error) string {
	return "\n\nOutput file retained after cleanup failure: " + record.OutputPath + " (" + err.Error() + ")"
}

func (t *bashTool) spillNotice(record backgroundprocess.Process) string {
	return "Output exceeded the inline limit (" + fmt.Sprintf("%.1f", float64(record.OutputSize)/1024) +
		" KB); read a suffix of " + record.OutputPath + " with the tail tool"
}

func bashTitle(command string) string {
	if len(command) <= 50 {
		return command
	}

	return command[:50] + "..."
}

func (t *bashTool) failureHint(output string) string {
	if hint := sandboxHint(output, t.runner.WritableRoots(), t.runner.ReadScope(), t.workDir); hint != "" {
		return hint
	}

	return ""
}

func (t *bashTool) backgroundedResult(record backgroundprocess.Process, automatic bool) *tool.Result {
	status := "Background execution was requested, so the command is now running in the background."
	if automatic {
		status = "The command was still running after 10 seconds, so it was moved to the background and is still running."
	}

	return &tool.Result{
		Title: "background process started",
		Output: status +
			"\nBackground process ID (not an operating-system PID): " + record.ID +
			"\nOutput file: " + record.OutputPath +
			"\nIf this command is wrong, stuck, redundant, or no longer needed, stop it with cancel_process using this background process ID." +
			"\nThe final result will arrive automatically in a new turn; do not poll." +
			"\nDo not poll with Bash, ps, sleep, schedule, Read, or Tail." +
			"\nDo not poll with tools or launch an overlapping command for the same goal; continue only useful independent work." +
			"\nWhen this is your only remaining work, reply with a standalone <WAITING/> line and no tool calls." +
			"\nIf you would otherwise poll, reply with a standalone I_WOULD_USE_<WAITING/> line and no tool calls instead.",
		Metadata: map[string]any{
			metaKeyProcessID: record.ID,
		},
	}
}

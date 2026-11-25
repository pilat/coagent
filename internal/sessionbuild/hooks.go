package sessionbuild

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/hooks"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/tool/builtin"
)

var _ session.StopHook = (*hooks.Hooks)(nil)

func loadHooks(ctx context.Context, in BuildInput, stack *builtin.Stack) *hooks.Hooks {
	launch := func(ctx context.Context, process hooks.Process) (hooks.Outcome, error) {
		out, err := stack.RunProcess(
			ctx,
			builtin.ProcessRun{
				Argv:  process.Argv,
				Env:   process.Env,
				Stdin: process.Stdin,
			},
		)

		result := hooks.Outcome{
			Stdout:   out.Stdout,
			Stderr:   out.Stderr,
			ExitCode: out.ExitCode,
		}
		if err != nil {
			return result, fmt.Errorf("run hook process: %w", err)
		}

		return result, nil
	}

	loaded, err := hooks.Load(ctx, stack.WorkDir(), in.Record.ID, launch)
	if err != nil {
		logger.Ctx(ctx).Warn("hooks_load_failed", zap.Error(err))
		return nil
	}

	return loaded
}

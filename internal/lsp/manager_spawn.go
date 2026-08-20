package lsp

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/pilat/coagent/internal/shellenv"
)

func (m *manager) wrappedServerCommand(
	ctx context.Context,
	server *serverConfig,
	root string,
) (*exec.Cmd, error) {
	cmd, err := m.spawnServer(ctx, server, root)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", server.ID, err)
	}

	if m.provider != nil {
		cmd, err = m.provider.WrapExec(ctx, root, cmd.Args, nil)
		if err != nil {
			return nil, fmt.Errorf("wrap %s spawn: %w", server.ID, err)
		}
	}

	// Resolution remains cancellable; after that, the manager owns process life.
	cmd.Cancel = nil
	cmd.WaitDelay = 0

	return cmd, nil
}

func (m *manager) spawnServer(ctx context.Context, server *serverConfig, root string) (*exec.Cmd, error) {
	if server.Spawn != nil {
		return server.Spawn(ctx, root)
	}

	if len(server.PathNames) == 0 {
		return nil, fmt.Errorf("server %s has no launch method", server.ID)
	}

	path, err := lookupExecutable(ctx, m.provider, root, server.PathNames)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not on the activated PATH for %s: %w",
			ErrServerUnavailable,
			strings.Join(server.PathNames, " or "),
			root,
			err,
		)
	}

	return exec.CommandContext(ctx, path, server.Args...), nil
}

func lookupExecutable(
	ctx context.Context,
	environment shellenv.Provider,
	workDir string,
	names []string,
) (string, error) {
	if environment != nil {
		path, err := environment.LookPath(ctx, workDir, names)
		if err != nil {
			return "", fmt.Errorf("look up activated executable: %w", err)
		}

		return path, nil
	}

	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("executable not found: %s", strings.Join(names, ", "))
}

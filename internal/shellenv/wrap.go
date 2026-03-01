package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// WrapExec builds a command that sources workDir's snapshot then exec-replaces
// the shell with argv, so the server inherits the activated env/PATH but runs as
// the server process (not a shell wrapper). With no snapshot it returns a plain
// exec of argv. Because `source` runs after cmd.Env is established, snapshot vars
// win on name collision (notably PATH — intended); non-colliding extraEnv survives.
func (p *provider) WrapExec(
	ctx context.Context,
	workDir string,
	argv, extraEnv []string,
) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("shellenv: empty argv")
	}

	env := append(os.Environ(), extraEnv...)

	snap := p.Snapshot(ctx, workDir)
	if snap == "" {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workDir
		cmd.Env = env

		return cmd, nil
	}

	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}

	line := "source " + shellQuote(snap) + "; exec " + strings.Join(quoted, " ")

	cmd := exec.CommandContext(ctx, p.shell, "-c", line)
	cmd.Dir = workDir
	cmd.Env = env

	return cmd, nil
}

// shellQuote single-quotes s for safe folding into a `-c` string. Own copy:
// mirrors bashsandbox.shellPath — importing it would invert the dep direction.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

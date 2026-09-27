//go:build linux

package bashsandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// Clear ambient capabilities at exec rather than on an arbitrary Go thread.
// The launcher must also be unreachable through writable paths.
func restrictLauncherCapabilities(command *exec.Cmd, policy sandboxpolicy.Policy) error {
	holds, err := procexec.HoldsCapabilities()
	if err != nil {
		return fmt.Errorf("inspect Bubblewrap launcher capabilities: %w", err)
	}

	if !holds {
		return nil
	}

	info, err := os.Stat(procexec.CapabilityLauncher)
	if err != nil {
		return fmt.Errorf("inspect capability launcher: %w", err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("inspect capability launcher ownership")
	}

	nested, err := inUserNamespace()
	if err != nil {
		return err
	}

	if err := validateBubblewrapExecutable(
		procexec.CapabilityLauncher,
		info.Mode(),
		stat.Uid,
		nested,
		writableLauncherPaths(policy, procexec.CapabilityLauncher),
	); err != nil {
		return fmt.Errorf("unsafe capability launcher: %w", err)
	}

	command.Path = procexec.CapabilityLauncher
	command.Args = procexec.LauncherArgv(procexec.CapabilityLauncher, command.Args)

	return nil
}

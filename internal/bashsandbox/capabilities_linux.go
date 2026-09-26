//go:build linux

package bashsandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/pilat/coagent/internal/procexec"
)

// Ambient capabilities belong to the orchestrator, not Bubblewrap. Clearing
// them in an exec stage avoids changing one arbitrary Go runtime thread.
// Bubblewrap gets the sandbox's own provenance check rather than the generic
// one: its launcher must be unreachable from every writable root as well.
func restrictLauncherCapabilities(command *exec.Cmd, writableRoots []string) error {
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
		writableRoots,
	); err != nil {
		return fmt.Errorf("unsafe capability launcher: %w", err)
	}

	command.Path = procexec.CapabilityLauncher
	command.Args = procexec.LauncherArgv(procexec.CapabilityLauncher, command.Args)

	return nil
}

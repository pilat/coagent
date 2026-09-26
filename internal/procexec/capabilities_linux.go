//go:build linux

package procexec

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// CapabilityLauncher drops capabilities in an exec stage. Clearing them in the
// daemon would change one arbitrary Go runtime thread instead of the child.
const CapabilityLauncher = "/usr/bin/setpriv"

// loaderVariables reach the dynamic loader before the launcher can drop
// anything, so a workload that needs one must set it after activation.
var loaderVariables = []string{"GCONV_PATH", "GLIBC_TUNABLES", "LOCPATH", "MALLOC_TRACE", "NLSPATH"}

// Unprivileged keeps a nonroot orchestrator's capabilities from reaching a host
// workload. Call it after preparing Env; setup failures surface from Cmd.Start.
func Unprivileged(command *exec.Cmd) *exec.Cmd {
	if err := restrictCapabilities(command); err != nil {
		command.Err = errors.Join(command.Err, err)
	}

	return command
}

// HoldsCapabilities reports whether this process carries capabilities a child
// would inherit. A root orchestrator has nothing to strip.
func HoldsCapabilities() (bool, error) {
	if os.Geteuid() == 0 {
		return false, nil
	}

	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}

	var capabilities [2]unix.CapUserData
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		return false, fmt.Errorf("inspect launcher capabilities: %w", err)
	}

	return capabilities[0].Permitted != 0 || capabilities[1].Permitted != 0, nil
}

// LauncherArgv prefixes a command so the launcher empties the ambient and
// inheritable sets before the program runs.
func LauncherArgv(launcher string, argv []string) []string {
	return append([]string{launcher, "--ambient-caps=-all", "--inh-caps=-all", "--"}, argv...)
}

func restrictCapabilities(command *exec.Cmd) error {
	// An already-failed command never starts, and the launcher must not wrap
	// itself.
	if command.Err != nil || command.Path == CapabilityLauncher {
		return nil //nolint:nilerr // A prior Cmd.Err is the caller's to report, not ours to replace.
	}

	holds, err := HoldsCapabilities()
	if err != nil {
		return err
	}

	if !holds {
		return nil
	}

	return relaunchUnprivileged(command, CapabilityLauncher)
}

// relaunchUnprivileged rewrites the command to run through the launcher. The
// launcher must be one only root can replace, or it becomes the escalation.
func relaunchUnprivileged(command *exec.Cmd, launcher string) error {
	if err := checkLauncher(launcher); err != nil {
		return err
	}

	if err := checkLoaderEnvironment(command); err != nil {
		return err
	}

	command.Args = LauncherArgv(launcher, append([]string{command.Path}, command.Args[1:]...))
	command.Path = launcher

	return nil
}

func checkLauncher(launcher string) error {
	info, err := os.Stat(launcher)
	if err != nil {
		return fmt.Errorf("inspect capability launcher: %w", err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("capability launcher ownership is unavailable")
	}

	if stat.Uid != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("capability launcher %s must be a root-owned, non-writable executable", launcher)
	}

	return nil
}

func checkLoaderEnvironment(command *exec.Cmd) error {
	for _, entry := range command.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if value == "" {
			continue
		}

		if strings.HasPrefix(name, "LD_") || slices.Contains(loaderVariables, name) {
			return fmt.Errorf("privileged workload launcher cannot accept %s; set it after shell activation", name)
		}
	}

	return nil
}

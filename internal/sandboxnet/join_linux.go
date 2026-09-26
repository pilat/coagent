//go:build linux

package sandboxnet

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// RunJoin joins the tree's network namespace on a locked thread and replaces
// itself with argv. Only this thread survives the exec, so the join cannot leak
// into the runtime's other threads, and the capabilities that made the join
// possible are gone before the command starts.
func RunJoin(cfg JoinConfig, argv []string) error {
	if len(argv) == 0 {
		return errors.New("network join requires a command to exec")
	}

	if cfg.NetNSFD < 3 {
		return errors.New("network join requires a namespace handle")
	}

	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	if err := unix.Setns(cfg.NetNSFD, unix.CLONE_NEWNET); err != nil {
		_ = unix.Close(cfg.NetNSFD)

		return fmt.Errorf("join network namespace: %w", err)
	}

	_ = unix.Close(cfg.NetNSFD)

	if err := dropCapabilities(); err != nil {
		return err
	}

	if err := unix.CloseRange(3, ^uint(0), 0); err != nil {
		return fmt.Errorf("close setup descriptors: %w", err)
	}

	if err := unix.Exec(argv[0], argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", argv[0], err)
	}

	return nil
}

// dropCapabilities removes every capability from the bounding, permitted,
// effective, inheritable and ambient sets. The bounding set matters most: after
// exec it is the only thing that could let a command regain a capability.
func dropCapabilities() error {
	for capability := 0; capability <= maxCapability; capability++ {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("drop capability %d from bounding set: %w", capability, err)
		}
	}

	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}

	var data [2]unix.CapUserData

	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear capability sets: %w", err)
	}

	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}

	return nil
}

// maxCapability bounds the bounding-set sweep. The kernel reports EINVAL past
// its own maximum, which the sweep tolerates.
const maxCapability = 64

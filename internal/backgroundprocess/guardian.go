package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

const (
	guardianMode  = "__coagent_process_guardian"
	guardianReady = "READY"
)

// RunGuardian holds one process-group lease. The binary enters it before CLI
// parsing and returns only after lease loss or setup failure.
func RunGuardian(args []string) (bool, error) {
	if len(args) == 0 || args[0] != guardianMode {
		return false, nil
	}

	if len(args) != 2 || args[1] == "" {
		return true, errors.New("process guardian requires one guard path")
	}

	ready := os.NewFile(3, "guardian-ready")

	lease := os.NewFile(4, "guardian-lease")
	if ready == nil || lease == nil {
		return true, errors.New("process guardian pipes are unavailable")
	}
	defer ready.Close()
	defer lease.Close()

	guard, err := os.OpenFile(args[1], os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_, _ = fmt.Fprintf(ready, "ERROR %v\n", err)

		return true, fmt.Errorf("open process guard: %w", err)
	}
	defer guard.Close()

	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX); err != nil {
		_, _ = fmt.Fprintf(ready, "ERROR %v\n", err)

		return true, fmt.Errorf("lock process guard: %w", err)
	}
	defer syscall.Flock(int(guard.Fd()), syscall.LOCK_UN) //nolint:errcheck // Process exit releases it too.

	if _, err := ready.WriteString(guardianReady + "\n"); err != nil {
		return true, fmt.Errorf("signal process guardian readiness: %w", err)
	}

	_ = ready.Close()

	_, _ = io.Copy(io.Discard, lease)

	if err := syscall.Kill(-os.Getpid(), syscall.SIGKILL); err != nil {
		return true, fmt.Errorf("kill leased process group: %w", err)
	}

	return true, errors.New("process guardian survived group kill")
}

func newGuardianCommand(guardPath string, readyWriter, leaseReader *os.File) *exec.Cmd {
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}

	cmd := exec.CommandContext( //nolint:gosec // The executable is this process image.
		context.Background(), executable, guardianMode, guardPath,
	)
	cmd.ExtraFiles = []*os.File{readyWriter, leaseReader}

	return cmd
}

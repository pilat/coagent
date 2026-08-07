package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/pilat/coagent/internal/install"
)

// geteuid is the seam the escalation tests replace: a test process is never
// root, so the "already privileged" branch is otherwise unreachable.
var geteuid = os.Geteuid

// runDaemonVerb is the single escalation gate: every lifecycle verb writes to
// /etc or the system launchd domain, so runServiceAction stays privilege-blind.
func runDaemonVerb(ctx context.Context, action string) int {
	if shouldEscalate(action) {
		return escalate(ctx, action)
	}

	return runServiceAction(ctx, action)
}

// escalate re-invokes this binary under sudo for one verb. There is no fallback:
// a machine whose owner declines sudo gets no daemon.
func escalate(ctx context.Context, action string) int {
	if err := sudoCommand(ctx, action).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sudo coagent daemon %s: %v\nrun it yourself to see why\n", action, err)

		return exitError
	}

	return exitOK
}

func shouldEscalate(action string) bool {
	return needsRoot(action) && geteuid() != 0
}

// sudoCommand builds the re-exec. stdio is passed through so sudo prompts on the
// terminal that is already here.
func sudoCommand(ctx context.Context, action string) *exec.Cmd {
	// Path captured at boot, verb one of needsRoot's constants — neither is input.
	//nolint:gosec // G702: re-executing this same binary with a fixed verb
	cmd := exec.CommandContext(ctx, "sudo", selfExecPath, "daemon", action)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd
}

func needsRoot(action string) bool {
	switch action {
	case install.ActionInstall, install.ActionUninstall,
		install.ActionStart, install.ActionStop, install.ActionRestart:
		return true
	default:
		return false
	}
}

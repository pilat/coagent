package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/install"
)

// runServiceAction performs one daemon lifecycle verb. It is privilege-blind:
// runDaemonVerb has already made sure this process has the rights it needs.
func runServiceAction(ctx context.Context, action string) int {
	mgr, err := install.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	if err := perform(ctx, mgr, action); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	report(mgr.Info(), action)

	return exitOK
}

func perform(ctx context.Context, mgr install.Manager, action string) error {
	switch action {
	case install.ActionInstall:
		return mgr.Install(ctx)
	case install.ActionUninstall:
		return mgr.Uninstall(ctx)
	case install.ActionStart:
		return mgr.Start(ctx)
	case install.ActionStop:
		return mgr.Stop(ctx)
	case install.ActionRestart:
		return mgr.Restart(ctx)
	default:
		return fmt.Errorf("unknown daemon command %q", action)
	}
}

func report(info install.Info, action string) {
	if action == install.ActionUninstall {
		fmt.Printf("removed %s and %s — ~/%s is untouched\n", info.UnitPath, info.BinaryPath, coagenthome.DirName)

		return
	}

	fmt.Printf("%s · %s (%s) · %s\n", action, info.UnitName, info.Scope, info.BinaryPath)
	fmt.Printf("logs  %s\n", info.LogCommand)
}

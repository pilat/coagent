package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pilat/coagent/internal/install"
)

const daemonUsage = `Usage:
  coagent daemon                      run the daemon in the foreground
  coagent daemon install              set up and start the service
  coagent daemon uninstall            stop and remove the service
  coagent daemon start|stop|restart

The lifecycle verbs need root and re-exec themselves under sudo when they don't
have it.
`

// runDaemonCommand handles `coagent daemon` and its lifecycle subcommands. Bare
// `daemon` runs in the foreground — that is what the unit's ExecStart invokes.
func runDaemonCommand(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return bootDaemon(ctx)
	}

	switch args[0] {
	case install.ActionInstall, install.ActionUninstall, install.ActionStart, install.ActionStop, install.ActionRestart:
		silenceLogs()

		return runDaemonVerb(ctx, args[0])
	case "help", "-h", "--help":
		fmt.Print(daemonUsage)

		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown daemon command %q\n\n%s", args[0], daemonUsage)

		return exitError
	}
}

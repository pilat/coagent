package main

import (
	"context"
	"fmt"
	"os"
)

const daemonUsage = `Usage:
  coagent daemon          run the daemon in the foreground

This is what the service unit's ExecStart invokes. The lifecycle verbs are
top-level commands: coagent install|uninstall|start|stop|restart.
`

// runDaemonCommand handles `coagent daemon`. It runs the daemon in the
// foreground — that is what the unit's ExecStart invokes. Lifecycle verbs live
// at the top level, not behind this word.
func runDaemonCommand(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return bootDaemon(ctx)
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(daemonUsage)

		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown daemon command %q\n\n%s", args[0], daemonUsage)

		return exitError
	}
}

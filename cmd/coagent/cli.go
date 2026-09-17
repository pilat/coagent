package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/version"
)

// Exit codes. `status` distinguishes "not running" from "could not ask", because
// a supervisor script has to treat those differently.
const (
	exitOK         = 0
	exitError      = 1
	exitNotRunning = 2
)

// devVersion is what an untagged build reports. It is incomparable on purpose:
// a dev binary makes no claim about being older or newer than a release.
const devVersion = "dev"

const usage = `coagent — a self-hosted headless coding agent.

Usage:
  coagent                 print this help
  coagent daemon          run the daemon in the foreground
  coagent status          report daemon state      (0 running, 2 not running, 1 error)
  coagent version         print the binary version
`

// dispatch routes argv. There is no flag library on purpose: the whole surface
// is a handful of verbs, and a dependency that parses them would be larger than
// the code that acts on them.
func dispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)

		return exitOK
	}

	switch args[0] {
	case "daemon":
		return runDaemonCommand(ctx, args[1:])
	case "status":
		silenceLogs()

		return runStatus(ctx)
	case "version":
		fmt.Println(version.Version)

		return exitOK
	case "help", "-h", "--help":
		fmt.Print(usage)

		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)

		return exitError
	}
}

// silenceLogs muzzles zap for the client verbs. The logger defaults to debug on
// stderr for the daemon's benefit; a CLI talks to its user with fmt.
func silenceLogs() {
	logger.Init(logger.WithConsoleOutput(io.Discard))
}

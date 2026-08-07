package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/pilat/coagent/internal/ctl"
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
  coagent                 set up and chat with the daemon (requires a terminal)
  coagent daemon          run the daemon in the foreground
  coagent status          report daemon state      (0 running, 2 not running, 1 error)
  coagent version         print the binary version
`

// dispatch routes argv. There is no flag library on purpose: the whole surface
// is a handful of verbs, and a dependency that parses them would be larger than
// the code that acts on them.
func dispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		silenceLogs()

		return runBare(ctx)
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

// runBare is what `coagent` alone does. Without a terminal it prints usage and
// fails: the legacy service unit invoked the bare binary, and exiting 0 there
// would let the unit die silently instead of visibly failing.
func runBare(ctx context.Context) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprint(os.Stderr, usage)

		return exitError
	}

	return runOnboarding(ctx)
}

// runOnboarding is bare `coagent`: make sure there is a daemon worth talking to,
// then hand the terminal to the chat.
func runOnboarding(ctx context.Context) int {
	socket, err := ctl.SocketPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	if code := bootstrap(ctx, socket); code != exitOK {
		return code
	}

	return runChat(ctx, socket)
}

// silenceLogs muzzles zap for the client verbs. The logger defaults to debug on
// stderr for the daemon's benefit; a CLI talks to its user with fmt, and a chat
// REPL cannot share the terminal with a log stream.
func silenceLogs() {
	logger.Init(logger.WithHumanOutput(io.Discard))
}

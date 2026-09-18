package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/install"
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
  coagent install         set up and start the service
  coagent uninstall       stop and remove the service
  coagent start|stop|restart   control the service
  coagent daemon          run the daemon in the foreground
  coagent status          report daemon state
  coagent version         print the binary version

The lifecycle verbs need root and re-exec themselves under sudo when they don't
have it.
`

// dispatch routes argv. There is no flag library on purpose: the whole surface
// is a handful of verbs, and a dependency that parses them would be larger than
// the code that acts on them.
func dispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		printBuildInfo(ctx)
		fmt.Print(usage)

		return exitOK
	}

	switch args[0] {
	case install.ActionInstall, install.ActionUninstall,
		install.ActionStart, install.ActionStop, install.ActionRestart:
		silenceLogs()

		return runDaemonVerb(ctx, args[0])
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

// printBuildInfo heads the bare-invocation screen with this binary's version and
// the installed service binary's — or that no service is installed. The installed
// version comes from execing the on-disk binary, so it reflects the file itself,
// not whatever a running daemon happens to be on.
func printBuildInfo(ctx context.Context) {
	fmt.Printf("this binary  %s\n", version.Version)

	mgr, err := install.New()
	if err != nil {
		return
	}

	info := mgr.Info()
	if !info.Supported {
		return
	}

	if !info.Installed {
		fmt.Println("installed    not installed — `sudo coagent install` sets it up")

		return
	}

	got := installedVersion(ctx, info.BinaryPath)
	fmt.Printf("installed    %s at %s\n", got, info.BinaryPath)

	if skewed(got, version.Version) {
		fmt.Println("             ⚠ differs from this binary — `sudo coagent restart` to apply")
	}
}

// installedVersion reads the on-disk binary's version by running it. `version`
// touches no DB, socket, or network, so this is a few milliseconds; the timeout
// only guards a path that hangs, never the normal case.
func installedVersion(ctx context.Context, binaryPath string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binaryPath, "version").Output()
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(out))
}

// silenceLogs muzzles zap for the client verbs. The logger defaults to debug on
// stderr for the daemon's benefit; a CLI talks to its user with fmt.
func silenceLogs() {
	logger.Init(logger.WithConsoleOutput(io.Discard))
}

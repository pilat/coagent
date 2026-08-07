package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/version"
)

// runStatus reports daemon state. Exit codes are the contract: 0 running,
// 2 not running, not installed or not ready yet, 1 could not ask.
func runStatus(ctx context.Context) int {
	path, err := ctl.SocketPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	return statusOf(ctx, path)
}

func statusOf(ctx context.Context, socket string) int {
	client, err := ctl.Dial(ctx, socket)
	if err != nil {
		return reportStatusFailure(err)
	}

	defer func() { _ = client.Close() }()

	st, err := client.Status(ctx)
	if err != nil {
		return reportStatusFailure(err)
	}

	printStatus(st, client.Greeting())

	return exitOK
}

// reportStatusFailure names the state instead of printing a transport error.
// A booting daemon is retryable, like a missing one — not "could not ask".
func reportStatusFailure(err error) int {
	if errors.Is(err, ctl.ErrNotRunning) {
		fmt.Println("daemon not running")

		return exitNotRunning
	}

	if errors.Is(err, ctl.ErrStarting) {
		fmt.Println("daemon starting — not answering yet")

		return exitNotRunning
	}

	fmt.Fprintf(os.Stderr, "%v\n", err)

	return exitError
}

func printStatus(st ctl.StatusResult, g ctl.Greeting) {
	fmt.Printf("running · pid %d · up %s · %s\n", st.PID, humanDuration(st.UptimeSeconds), st.BinaryVersion)

	if skewed(g.BinaryVersion, version.Version) {
		fmt.Printf("version skew · daemon %s ≠ cli %s — `coagent` offers the update\n",
			g.BinaryVersion, version.Version)
	}

	if !st.ConfigPresent {
		fmt.Printf("config  none yet — %s\n", st.ConfigPath)

		return
	}

	fmt.Printf("config  %s\n", st.ConfigPath)

	for _, p := range st.Providers {
		fmt.Printf("provider %s (%s)\n", p.Name, p.Driver)
	}

	fmt.Printf("models  %d · default %s\n", st.ModelCount, orNone(st.DefaultModel))

	for _, m := range st.Managers {
		fmt.Println(managerLine(m))
	}
}

func managerLine(m ctl.ManagerStatus) string {
	state := "disabled"

	switch {
	case m.Running:
		state = "running"
	case m.Enabled:
		state = "not running"
	}

	line := fmt.Sprintf("manager %s (%s) · %s", m.ID, m.Driver, state)
	if m.Error != "" {
		line += " · " + m.Error
	}

	return line
}

// skewed compares the daemon's binary version with this one. A "dev" build on
// either side is incomparable — an untagged binary makes no claim about being
// older or newer, so there is nothing to offer.
func skewed(daemonVersion, cliVersion string) bool {
	if daemonVersion == devVersion || cliVersion == devVersion {
		return false
	}

	return daemonVersion != cliVersion
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}

func humanDuration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	}
}

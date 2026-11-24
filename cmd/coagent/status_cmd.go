package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/version"
)

var statusTimeout = 3 * time.Second

// runStatus reports daemon state. Exit codes are the contract: 0 running,
// 2 not running, not installed or not ready yet, 1 could not ask.
func runStatus(ctx context.Context) int {
	path, err := coagenthome.Join(coagenthome.SocketFileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	return statusOf(ctx, path)
}

func statusOf(ctx context.Context, socket string) int {
	st, connected, err := fetchStatus(ctx, socket)
	if err != nil {
		return reportStatusFailure(err, connected)
	}

	printStatus(st)

	return exitOK
}

func fetchStatus(ctx context.Context, socket string) (*coagentv1.GetStatusResponse, bool, error) {
	var connected atomic.Bool

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer

		conn, err := dialer.DialContext(ctx, "unix", socket)
		if err == nil {
			connected.Store(true)
		}

		return conn, err
	}}

	defer transport.CloseIdleConnections()

	client := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://coagent"))
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)

	defer cancel()

	status, err := coagentv1connect.NewDaemonServiceClient(client).GetStatus(ctx, &coagentv1.GetStatusRequest{})

	return status, connected.Load(), err
}

// reportStatusFailure names the state instead of printing a transport error.
// A booting daemon is retryable, like a missing one — not "could not ask".
func reportStatusFailure(err error, connected bool) int {
	if !connected {
		fmt.Println("daemon not running")

		return exitNotRunning
	}

	if isStarting(err) {
		fmt.Println("daemon starting — not answering yet")

		return exitNotRunning
	}

	fmt.Fprintf(os.Stderr, "%v\n", err)
	fmt.Fprintln(
		os.Stderr,
		"the daemon may be running an older coagent binary — `sudo coagent restart` to apply this one",
	)

	return exitError
}

func isStarting(err error) bool {
	var remote *connect.Error

	return errors.As(err, &remote) && remote.IsRemote() && remote.Code() == connect.CodeUnavailable ||
		errors.Is(err, context.DeadlineExceeded)
}

func printStatus(st *coagentv1.GetStatusResponse) {
	fmt.Printf(
		"running · pid %d · up %s · %s\n",
		st.GetPid(),
		humanDuration(st.GetUptimeSeconds()),
		st.GetBinaryVersion(),
	)

	if skewed(st.GetBinaryVersion(), version.Version) {
		fmt.Printf("version skew · daemon %s ≠ cli %s — restart the daemon on the new binary\n",
			st.GetBinaryVersion(), version.Version)
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

	for _, id := range st.StaleModels {
		fmt.Printf("model %s (stale catalog)\n", id)
	}

	for _, m := range st.Managers {
		fmt.Println(managerLine(m))
	}
}

func managerLine(m *coagentv1.ManagerStatus) string {
	state := "disabled"

	switch {
	case m.Running:
		state = "running"
	case m.Enabled:
		state = "not running"
	}

	line := fmt.Sprintf("manager %s (%s) · %s", m.GetId(), m.GetDriver(), state)
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

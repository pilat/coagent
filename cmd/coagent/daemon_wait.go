package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pilat/coagent/internal/ctl"
)

// daemonWait bounds how long a fresh or restarted daemon gets to answer; past it,
// saying so beats spinning. A var only so tests can shrink it.
var daemonWait = 60 * time.Second

// waitForDaemon polls the socket until any daemon greets, bounded.
func waitForDaemon(ctx context.Context, socket string) (ctl.StatusResult, int) {
	return waitForDaemonWhere(ctx, socket, func(ctl.Greeting, ctl.StatusResult) bool { return true })
}

// waitForVersion is the same poll pinned to one binary. An update needs it: the
// old daemon answers on the same socket until its drain closes the listener.
func waitForVersion(ctx context.Context, socket, want string) (ctl.StatusResult, int) {
	return waitForDaemonWhere(ctx, socket, func(g ctl.Greeting, _ ctl.StatusResult) bool {
		return g.BinaryVersion == want
	})
}

// waitForReboot waits for a run of the daemon other than the one named: a restart
// reuses the pid and socket, so the boot id is the only thing separating them.
func waitForReboot(ctx context.Context, socket, previous string) (ctl.StatusResult, int) {
	// A daemon too old to report a boot id leaves nothing to compare, and waiting
	// for a signal it will never send is worse than the plain wait.
	if previous == "" {
		return waitForDaemon(ctx, socket)
	}

	return waitForDaemonWhere(ctx, socket, func(_ ctl.Greeting, st ctl.StatusResult) bool {
		return st.BootID != previous
	})
}

func waitForDaemonWhere(
	ctx context.Context, socket string, accept func(ctl.Greeting, ctl.StatusResult) bool,
) (ctl.StatusResult, int) {
	deadline := time.Now().Add(daemonWait)

	for time.Now().Before(deadline) {
		if st, ok := askDaemon(ctx, socket, accept); ok {
			return st, exitOK
		}

		select {
		case <-ctx.Done():
			return ctl.StatusResult{}, exitError
		case <-time.After(reconnectPoll):
		}
	}

	fmt.Fprintf(os.Stderr, "the daemon did not come up within %s — check `coagent status`\n", daemonWait)

	return ctl.StatusResult{}, exitNotRunning
}

// askDaemon is one poll: connect, read status, and let the caller judge who
// answered.
func askDaemon(
	ctx context.Context, socket string, accept func(ctl.Greeting, ctl.StatusResult) bool,
) (ctl.StatusResult, bool) {
	client, err := ctl.Dial(ctx, socket)
	if err != nil {
		return ctl.StatusResult{}, false
	}

	defer func() { _ = client.Close() }()

	st, err := client.Status(ctx)
	if err != nil {
		return ctl.StatusResult{}, false
	}

	return st, accept(client.Greeting(), st)
}

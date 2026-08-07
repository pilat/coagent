package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/install"
	"github.com/pilat/coagent/internal/version"
)

// bootstrap is the deterministic half of a first run: make sure a daemon is
// listening, that it is not an old image, and that it has at least one provider
// — so the chat that does everything else has a model to run on.
func bootstrap(ctx context.Context, socket string) int {
	st, code := ensureDaemon(ctx, socket)
	if code != exitOK {
		return code
	}

	if len(st.Providers) > 0 {
		return exitOK
	}

	return addFirstProvider(ctx, socket)
}

// ensureDaemon connects, installing or updating one as needed. A booting daemon is
// waited out, not reported broken: its socket answers before its managers are up.
func ensureDaemon(ctx context.Context, socket string) (ctl.StatusResult, int) {
	greeting, st, err := readDaemonState(ctx, socket)

	if errors.Is(err, ctl.ErrNotRunning) {
		if code := offerInstall(ctx); code != exitOK {
			return ctl.StatusResult{}, code
		}

		return waitForDaemon(ctx, socket)
	}

	if errors.Is(err, ctl.ErrStarting) {
		fmt.Println("The daemon is starting…")

		if _, code := waitForDaemon(ctx, socket); code != exitOK {
			return ctl.StatusResult{}, code
		}

		greeting, st, err = readDaemonState(ctx, socket)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return ctl.StatusResult{}, exitError
	}

	if !skewed(greeting.BinaryVersion, version.Version) {
		return st, exitOK
	}

	return offerUpdate(ctx, socket, greeting.BinaryVersion, st)
}

// readDaemonState is one connect plus one status read — the pair every caller
// here wants, and the pair that tells "down" from "starting" from "ready".
func readDaemonState(ctx context.Context, socket string) (ctl.Greeting, ctl.StatusResult, error) {
	client, err := ctl.Dial(ctx, socket)
	if err != nil {
		return ctl.Greeting{}, ctl.StatusResult{}, err
	}

	defer func() { _ = client.Close() }()

	st, err := client.Status(ctx)
	if err != nil {
		return ctl.Greeting{}, ctl.StatusResult{}, err
	}

	return client.Greeting(), st, nil
}

// offerInstall registers the service. Bare `coagent` runs the install itself:
// the terminal is right here, so sudo can ask for a password.
func offerInstall(ctx context.Context) int {
	fmt.Println("No coagent daemon is running.")

	if !confirm("Install and start it now? (needs sudo)") {
		fmt.Println("Nothing installed. `sudo coagent daemon install` sets it up whenever you are ready.")

		return exitNotRunning
	}

	return runDaemonVerb(ctx, install.ActionInstall)
}

// offerUpdate swaps the binary and restarts the daemon onto it, both as the
// plain user. Declining keeps the status read: an old image is still configured.
func offerUpdate(ctx context.Context, socket, daemonVersion string, current ctl.StatusResult) (ctl.StatusResult, int) {
	fmt.Printf("The running daemon is %s; this binary is %s.\n", daemonVersion, version.Version)

	if !confirm("Restart the daemon onto this binary?") {
		return current, exitOK
	}

	if err := install.UpdateBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return current, exitError
	}

	warnUnitDrift()

	if code := requestRestart(ctx, socket); code != exitOK {
		return current, code
	}

	return waitForVersion(ctx, socket, version.Version)
}

// requestRestart hands the restart to the daemon itself. The install fallback is
// the only path here that can reach for a password.
func requestRestart(ctx context.Context, socket string) int {
	client, err := ctl.Dial(ctx, socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	var res ctl.RestartResult

	err = client.Call(ctx, ctl.OpRestartDaemon, nil, &res)

	_ = client.Close()

	if err == nil {
		return exitOK
	}

	if unknownMethod(err) {
		return runDaemonVerb(ctx, install.ActionInstall)
	}

	fmt.Fprintf(os.Stderr, "%v\n", err)

	return exitError
}

// unknownMethod is the one error that means "this daemon is a version behind",
// as opposed to a daemon that is broken. Only it earns the sudo fallback.
func unknownMethod(err error) bool {
	var rpcErr *ctl.Error

	return errors.As(err, &rpcErr) && rpcErr.Code == ctl.CodeMethodNotFound
}

// warnUnitDrift reports a service file this version would write differently.
// Escalating over drift would undo the point of a sudo-free update.
func warnUnitDrift() {
	stale, err := install.UnitStale()
	if err != nil || !stale {
		return
	}

	fmt.Println(
		"note  the installed service file is missing or out of date — `sudo coagent daemon install` refreshes it",
	)
}

// addFirstProvider collects one provider and its key, then hands the rest to the
// chat. The fields are per-driver because the schema is: the daemon refuses to
// start on a provider missing what its driver needs.
func addFirstProvider(ctx context.Context, socket string) int {
	fmt.Println("\nNo LLM provider is configured yet. Let's add one — everything else happens in the chat.")

	params, ok := collectProvider()
	if !ok {
		return exitError
	}

	client, err := ctl.Dial(ctx, socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	defer func() { _ = client.Close() }()

	// The run of the daemon that takes the write: what comes back has to be a
	// different one, or the chat opens on a socket about to be unlinked.
	before, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	var v configops.Verdict
	if err := client.Call(ctx, ctl.OpSetProvider, params, &v); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	if !v.Applied {
		fmt.Fprintf(os.Stderr, "rejected — %s\n", v.Reason())

		return exitError
	}

	fmt.Println("Saved. Restarting the daemon…")

	st, code := waitForReboot(ctx, socket, before.BootID)
	if code != exitOK {
		return code
	}

	return reportFirstProvider(st, params.Name)
}

// reportFirstProvider checks the daemon came back with the provider it accepted.
// A rolled-back config has nobody to tell, so absence is the whole signal.
func reportFirstProvider(st ctl.StatusResult, name string) int {
	if slices.ContainsFunc(st.Providers, func(p ctl.ProviderStatus) bool { return p.Name == name }) {
		return exitOK
	}

	fmt.Fprintf(os.Stderr,
		"the daemon could not start with provider %q, so the change was rolled back — check `coagent status`, "+
			"then run `coagent` again with different details\n", name)

	return exitError
}

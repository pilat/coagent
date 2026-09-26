package sandboxnet

import (
	"errors"
	"strconv"
)

// ErrSetupUnsupported reports that this environment cannot build the namespaces
// the sandbox link needs.
var ErrSetupUnsupported = errors.New("sandbox network setup is unavailable in this environment")

// SetupName is the interface name the sandbox side of the link uses.
const SetupName = "coagent0"

// SetupCommand is the hidden subcommand that runs the setup child.
const SetupCommand = "__coagent_net_setup"

// SetupUnsupportedExit is the code the setup child exits with when the
// environment forbids the namespaces it needs — a supervisor that pivoted the
// root, or a container without unprivileged user namespaces. Sandbox startup
// fails rather than falling back to host networking.
const SetupUnsupportedExit = 77

// setupReturnFD is the first entry of the child's ExtraFiles.
const setupReturnFD = 3

// SetupConfig configures the child that holds one sandbox network namespace.
// The router addresses, routes and rules both ends from the parent, so the
// child carries no addressing of its own.
type SetupConfig struct {
	// ReturnFD is the socket the namespace descriptor is handed back on, and
	// whose closure retires the generation.
	ReturnFD int
}

// SetupArgs renders the hidden setup invocation. The descriptor the child
// returns the namespace on is always fd 3.
func SetupArgs() []string {
	return []string{SetupCommand, "--return-fd", strconv.Itoa(setupReturnFD)}
}

// RunSetupMode is the hidden setup entry point. It reports whether it handled
// the invocation, so the binary can continue to its ordinary dispatch.
func RunSetupMode(args []string) (bool, error) {
	if len(args) == 0 || args[0] != SetupCommand {
		return false, nil
	}

	config, err := ParseSetupArgs(args[1:])
	if err != nil {
		return true, err
	}

	return true, RunSetup(config)
}

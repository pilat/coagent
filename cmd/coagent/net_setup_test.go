package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/sandboxnet"
)

// TestRunModes_DispatchesTheNetworkSetupSubcommand proves the hidden setup mode
// is reachable through the mode chain and never falls through to the daemon
// dispatch. The invocation is deliberately malformed: a valid one would build
// namespaces in this process.
func TestRunModes_DispatchesTheNetworkSetupSubcommand(t *testing.T) {
	guardianCalled := false

	modes := runModes(
		func([]string) (bool, error) {
			guardianCalled = true

			return false, nil
		},
		sandboxnet.RunSetupMode,
		sandboxnet.RunJoinMode,
	)

	handled, err := modes([]string{sandboxnet.SetupCommand, "--address", "10.211.0.2"})
	require.True(t, handled, "the setup subcommand must be recognized")
	require.Error(t, err, "an incomplete setup invocation is rejected by its parser")
	assert.True(t, guardianCalled, "the guardian is consulted first and declines the setup invocation")

	handled, err = modes([]string{"daemon"})
	assert.False(t, handled, "an ordinary invocation stays with the daemon dispatch")
	assert.NoError(t, err)
}

// TestRunModes_DispatchesTheNetworkJoinSubcommand proves the join mode is
// reachable too, and that a bad namespace descriptor fails cleanly instead of
// leaving the process in an unknown state.
func TestRunModes_DispatchesTheNetworkJoinSubcommand(t *testing.T) {
	modes := runModes(
		func([]string) (bool, error) { return false, nil },
		sandboxnet.RunSetupMode,
		sandboxnet.RunJoinMode,
	)

	handled, err := modes([]string{sandboxnet.JoinCommand, "--netns-fd"})
	require.True(t, handled, "the join subcommand must be recognized")
	require.Error(t, err, "a missing descriptor value is rejected by the parser")

	handled, err = modes([]string{sandboxnet.JoinCommand, "--netns-fd", "999999", "--", "/bin/true"})
	require.True(t, handled)
	require.ErrorContains(t, err, "join network namespace", "an invalid namespace handle fails cleanly")
}

// TestRunModes_FirstHandlerWins pins the chain order: the guardian sees every
// invocation before the setup mode.
func TestRunModes_FirstHandlerWins(t *testing.T) {
	secondCalled := false

	modes := runModes(
		func([]string) (bool, error) { return true, nil },
		func([]string) (bool, error) {
			secondCalled = true

			return true, nil
		},
	)

	handled, err := modes([]string{"anything"})
	require.True(t, handled)
	require.NoError(t, err)
	assert.False(t, secondCalled)
}

func TestRunModes_DispatchesTheGuestDialSubcommand(t *testing.T) {
	modes := runModes(sandboxnet.RunSetupMode, sandboxnet.RunJoinMode, bashsandbox.RunDialMode)
	handled, err := modes([]string{bashsandbox.DialCommand})
	require.True(t, handled)
	require.ErrorContains(t, err, "network dial requires")
}

package sandboxnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupArgs_RoundTripsTheReturnDescriptor(t *testing.T) {
	args := SetupArgs()

	assert.Equal(t, SetupCommand, args[0])

	config, err := ParseSetupArgs(args[1:])
	require.NoError(t, err)
	assert.Equal(t, setupReturnFD, config.ReturnFD)
}

func TestParseSetupArgs_RejectsIncompleteInvocations(t *testing.T) {
	for name, args := range map[string][]string{
		"no fd":         {},
		"missing value": {"--return-fd"},
		"unknown flag":  {"--name", "coagent9"},
		"bad fd":        {"--return-fd", "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSetupArgs(args)
			require.Error(t, err)
		})
	}
}

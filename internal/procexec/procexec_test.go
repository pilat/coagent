package procexec

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFromCommandPreservesPreparedProcess(t *testing.T) {
	command := exec.CommandContext(context.Background(), "/usr/bin/tool", "arg")
	command.Dir = "/work"
	command.Env = []string{"KEY=value"}

	request, err := FromCommand(command)
	require.NoError(t, err)
	assert.Equal(t, Request{Path: "/usr/bin/tool", Args: []string{"arg"}, WorkDir: "/work", Env: []string{"KEY=value"}}, request)

	command.Args[1] = "changed"
	command.Env[0] = "CHANGED=value"
	assert.Equal(t, []string{"arg"}, request.Args)
	assert.Equal(t, []string{"KEY=value"}, request.Env)
}

func TestFromCommandRejectsIncompleteCommand(t *testing.T) {
	_, err := FromCommand(nil)
	require.Error(t, err)
}

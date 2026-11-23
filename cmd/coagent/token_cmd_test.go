package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/localapi"
)

func TestRunToken_PrintsTokenAndItsHash(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	require.Equal(t, exitOK, runToken([]string{"test-client"}, &out))
	lines := strings.Split(out.String(), "\n")
	require.Greater(t, len(lines), 2)
	token := lines[1]
	assert.Regexp(t, `^coagent_[A-Za-z0-9_-]{43}$`, token)
	assert.Equal(t, 1, strings.Count(out.String(), token))
	_, yaml, ok := strings.Cut(out.String(), "\napi:\n")
	require.True(t, ok)
	cfg, err := config.ParseAndResolve([]byte("api:\n"+yaml), nil)
	require.NoError(t, err)
	require.Len(t, cfg.API.Clients, 1)
	assert.Equal(t, "test-client", cfg.API.Clients[0].ID)
	assert.Equal(t, config.APIRoleOperator, cfg.API.Clients[0].Role)
	assert.Equal(t, localapi.TokenHash(token), cfg.API.Clients[0].TokenSHA256)
}

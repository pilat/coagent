package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestStartLocalAPI_WithoutClientsBindsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, unified := range []*config.UnifiedConfig{nil, {}} {
		a := &app{}
		startLocalAPI(t.Context(), a, &config.Config{UnifiedConfig: unified}, nil)
		_, err := os.Stat(filepath.Join(home, coagenthome.DirName, coagenthome.APISocketFileName))
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Empty(t, a.stops)
	}
}

func TestStartLocalAPI_BindFailureLeavesTheDaemonRunning(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "api-bind")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.APISocketFileName)
	require.NoError(t, os.MkdirAll(socket, 0o700))
	// A nonempty directory cannot be removed as a stale socket.
	require.NoError(t, os.WriteFile(filepath.Join(socket, "occupied"), []byte("keep"), 0o600))
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		API: config.APIConfig{Clients: []config.APIClientEntry{{
			ID: "test", Role: config.APIRoleOperator, TokenSHA256: strings.Repeat("a", 64),
		}}},
	}}
	a := &app{}
	require.NotPanics(t, func() { startLocalAPI(t.Context(), a, cfg, nil) })
	assert.Empty(t, a.stops)
	content, err := os.ReadFile(filepath.Join(socket, "occupied"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(content))
}

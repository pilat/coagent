package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStampWorkDirBindsAndFiltersDisabled(t *testing.T) {
	disabled := false

	got := stampWorkDir(map[string]ServerConfig{
		"on":       {Command: "run"},
		"off":      {Command: "run", Disabled: true},
		"off-flag": {Command: "run", Enabled: &disabled},
	}, "/work")

	require.Len(t, got, 1)
	assert.Equal(t, "/work", got["on"].WorkDir)
}

func TestAcquireForWorkDirWithNoServers(t *testing.T) {
	tests := []struct {
		name    string
		servers map[string]ServerConfig
	}{
		{name: "nil", servers: nil},
		{name: "empty", servers: map[string]ServerConfig{}},
		{name: "all disabled", servers: map[string]ServerConfig{"x": {Command: "run", Disabled: true}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := AcquireForWorkDir(context.Background(), nil, tt.servers, "/work", nil)
			require.NoError(t, err)
			assert.Nil(t, svc)
		})
	}
}

// The workdir is part of the pool's identity hash, so the same definition in two
// workdirs must not share a subprocess.
func TestStampWorkDirChangesTheIdentityHash(t *testing.T) {
	def := map[string]ServerConfig{"srv": {Command: "run", Args: []string{"-x"}}}

	a := stampWorkDir(def, "/one")["srv"]
	b := stampWorkDir(def, "/two")["srv"]

	assert.NotEqual(t, a.Hash(), b.Hash())
	assert.Equal(t, a.Hash(), stampWorkDir(def, "/one")["srv"].Hash())
}

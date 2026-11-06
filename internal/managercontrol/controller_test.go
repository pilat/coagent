package managercontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/managerdiscovery"
)

func TestSpawnFavorites(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		SpawnFavorites: []string{"/repo/two", "/repo/one"},
	}}
	discovery := managerdiscovery.New(nil, cfg, nil)
	ctrl := New(nil, nil, discovery, nil, cfg, nil).ForManager("test")

	got, err := ctrl.SpawnFavorites(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"/repo/two", "/repo/one"}, got)
}

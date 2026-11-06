package managerdiscovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestSpawnFavorites(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want []string
	}{
		{name: "no config", want: []string{}},
		{name: "no unified config", cfg: &config.Config{}, want: []string{}},
		{
			name: "unset", cfg: &config.Config{UnifiedConfig: &config.UnifiedConfig{}},
			want: []string{},
		},
		{
			name: "empty",
			cfg:  &config.Config{UnifiedConfig: &config.UnifiedConfig{SpawnFavorites: []string{}}},
			want: []string{},
		},
		{
			name: "configured",
			cfg: &config.Config{UnifiedConfig: &config.UnifiedConfig{
				SpawnFavorites: []string{"/repo/two", "/repo/one"},
			}},
			want: []string{"/repo/two", "/repo/one"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := New(nil, tt.cfg, nil)
			got, err := svc.SpawnFavorites(t.Context())
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got)
		})
	}
}

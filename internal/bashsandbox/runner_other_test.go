//go:build !darwin && !linux

package bashsandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_EnabledUnsupportedPlatform(t *testing.T) {
	_, err := New(Config{Enabled: true, WorkDir: t.TempDir()}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}
